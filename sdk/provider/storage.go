package provider

import (
	"context"
	"io"
	"time"
)

// ObjectMetadata describes an artifact stored in object storage.
type ObjectMetadata struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"contentType"`
	LastModified time.Time         `json:"lastModified"`
	CustomMeta   map[string]string `json:"customMeta,omitempty"`
}

// StorageProvider supplies object, file, and artifact persistence.
type StorageProvider interface {
	Provider
	PutObject(ctx context.Context, key string, data io.Reader, size int64, contentType string) (ObjectMetadata, error)
	GetObject(ctx context.Context, key string) (io.ReadCloser, ObjectMetadata, error)
	DeleteObject(ctx context.Context, key string) error
	ListObjects(ctx context.Context, prefix string, limit int) ([]ObjectMetadata, error)
}
