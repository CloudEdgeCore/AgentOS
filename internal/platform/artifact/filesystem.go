// Package artifact provides content-addressed Artifact storage adapters.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
)

var tenantToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Filesystem struct {
	root     string
	maxBytes int64
}

func NewFilesystem(root string, maxBytes int64) (*Filesystem, error) {
	if strings.TrimSpace(root) == "" || maxBytes <= 0 {
		return nil, fmt.Errorf("artifact root and positive maximum size are required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	return &Filesystem{root: absolute, maxBytes: maxBytes}, nil
}

func (f *Filesystem) Put(ctx context.Context, tenantID, mediaType string, source io.Reader) (store.ArtifactReference, error) {
	var result store.ArtifactReference
	if !tenantToken.MatchString(tenantID) || strings.TrimSpace(mediaType) == "" || source == nil {
		return result, fmt.Errorf("valid tenant, media type, and source are required")
	}
	tenantRoot := filepath.Join(f.root, tenantID, "sha256")
	if err := os.MkdirAll(tenantRoot, 0o700); err != nil {
		return result, fmt.Errorf("create tenant artifact directory: %w", err)
	}
	temporary, err := os.CreateTemp(tenantRoot, ".upload-*")
	if err != nil {
		return result, fmt.Errorf("create artifact staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	hash := sha256.New()
	written, copyErr := copyContext(ctx, io.MultiWriter(temporary, hash), io.LimitReader(source, f.maxBytes+1))
	// Sync before the rename publishes the artifact: a crash must never leave
	// a committed name pointing at unflushed content.
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil {
		return result, copyErr
	}
	if syncErr != nil {
		return result, fmt.Errorf("sync staged artifact: %w", syncErr)
	}
	if closeErr != nil {
		return result, fmt.Errorf("close staged artifact: %w", closeErr)
	}
	if written > f.maxBytes {
		return result, fmt.Errorf("artifact exceeds %d byte limit", f.maxBytes)
	}
	digest := hash.Sum(nil)
	digestHex := hex.EncodeToString(digest)
	finalPath := filepath.Join(tenantRoot, digestHex)
	if err := moveIfAbsent(temporaryPath, finalPath); err != nil {
		return result, err
	}
	copy(result.SHA256[:], digest)
	result.URI = "artifact://" + tenantID + "/sha256/" + digestHex
	result.SizeBytes = written
	result.MediaType = mediaType
	return result, nil
}

func (f *Filesystem) Open(ctx context.Context, tenantID string, reference store.ArtifactReference) (io.ReadCloser, error) {
	if !tenantToken.MatchString(tenantID) || reference.DigestHex() == strings.Repeat("0", sha256.Size*2) {
		return nil, fmt.Errorf("valid tenant and artifact digest are required")
	}
	expectedURI := "artifact://" + tenantID + "/sha256/" + reference.DigestHex()
	if reference.URI != expectedURI {
		return nil, fmt.Errorf("artifact URI does not match tenant and digest")
	}
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	default:
	}
	file, err := os.Open(filepath.Join(f.root, tenantID, "sha256", reference.DigestHex()))
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	return &verifyingReader{file: file, expected: reference.SHA256, expectedSize: reference.SizeBytes, hash: sha256.New()}, nil
}

type verifyingReader struct {
	file         *os.File
	expected     [sha256.Size]byte
	expectedSize int64
	read         int64
	hash         hashWriter
	verified     bool
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (r *verifyingReader) Read(buffer []byte) (int, error) {
	n, err := r.file.Read(buffer)
	if n > 0 {
		r.read += int64(n)
		_, _ = r.hash.Write(buffer[:n])
	}
	if err == io.EOF && !r.verified {
		r.verified = true
		if r.read != r.expectedSize || !strings.EqualFold(hex.EncodeToString(r.hash.Sum(nil)), hex.EncodeToString(r.expected[:])) {
			return n, fmt.Errorf("artifact content failed size or SHA-256 verification")
		}
	}
	return n, err
}

func (r *verifyingReader) Close() error { return r.file.Close() }

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, context.Cause(ctx)
		default:
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func moveIfAbsent(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect artifact destination: %w", err)
	}
	if err := os.Rename(source, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr == nil {
			return nil
		}
		return fmt.Errorf("commit artifact: %w", err)
	}
	return nil
}
