"""Materialise a remote commit's tree into the local worktree via the GitHub API.

The local git checkout cannot be moved reliably on this machine (every ref
update is silently rolled back), so instead of `git checkout <sha>` we fetch
each blob from the GitHub Git Data API and write the exact bytes to disk.

Usage:
  python sync_tree.py <sha>          dry run: report which files differ
  python sync_tree.py <sha> --write  fetch and overwrite the differing files
  python sync_tree.py <sha> --verify hash every tracked file and report mismatches

Blob identity is git's own: sha1(b"blob " + len + b"\0" + content). The repo
sets `* text=auto eol=lf`, so worktree bytes equal blob bytes and we can hash
files directly without applying clean filters.
"""
import base64
import hashlib
import json
import os
import subprocess
import sys

REPO = r"D:\OwnProject\AgentOS"
REPO_SLUG = "CloudEdgeCore/AgentOS"


def gh_json(args):
    out = subprocess.run(["gh"] + args, capture_output=True, cwd=REPO)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.decode("utf-8", "replace")[:500])
    return json.loads(out.stdout.decode("utf-8"))


def gh_raw(args):
    out = subprocess.run(["gh"] + args, capture_output=True, cwd=REPO)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.decode("utf-8", "replace")[:500])
    return out.stdout


def blob_sha(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def main():
    sha = sys.argv[1]
    mode = sys.argv[2] if len(sys.argv) > 2 else "--dry-run"

    tree = gh_json(
        [
            "api",
            f"repos/{REPO_SLUG}/git/trees/{sha}?recursive=1",
            "--paginate",
        ]
    )
    blobs = [n for n in tree["tree"] if n["type"] == "blob"]
    print(f"tree {sha}: {len(blobs)} blobs (truncated={tree.get('truncated')})")

    mismatched, missing = [], []
    for node in blobs:
        path = node["path"]
        local = os.path.join(REPO, path.replace("/", os.sep))
        if not os.path.exists(local):
            missing.append((path, node["sha"]))
            continue
        with open(local, "rb") as handle:
            data = handle.read()
        if blob_sha(data) != node["sha"]:
            mismatched.append((path, node["sha"]))

    print(f"missing  : {len(missing)}")
    for path, _ in missing:
        print(f"  M {path}")
    print(f"mismatched: {len(mismatched)}")
    for path, _ in mismatched:
        print(f"  ~ {path}")

    if mode == "--write":
        for path, node_sha in mismatched + missing:
            blob = gh_json([f"api", f"repos/{REPO_SLUG}/git/blobs/{node_sha}"])
            data = base64.b64decode(blob["content"])
            assert blob_sha(data) == node_sha, f"blob hash mismatch for {path}"
            local = os.path.join(REPO, path.replace("/", os.sep))
            os.makedirs(os.path.dirname(local), exist_ok=True)
            with open(local, "wb") as handle:
                handle.write(data)
            print(f"  wrote {path} ({len(data)} bytes)")
        print("write complete; re-run without --write to verify")
    return 0


if __name__ == "__main__":
    sys.exit(main())
