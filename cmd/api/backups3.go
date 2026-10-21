package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/offsite"
)

const backupS3Usage = `Usage: /api backup-s3 <command> [options]

The off-machine copy of the backups of a self-hosted installation in an S3
bucket (CLAB-132). backup.sh and clab server run it in a throwaway container
with the backups directory mounted; the bucket comes from BACKUP_S3_* in the
environment, the keys never from the command line.

Commands:
  put [--part-size BYTES] FILE   upload FILE (cadence-vX.Y.Z-<date>.sql.gz), the
                                 .blobs.tar.gz next to it and a .sha256 of both,
                                 then keep the newest BACKUP_S3_KEEP backups
  list                           the backups in the bucket, newest first
  get [--dir DIR] [--version V] [NAME]
                                 download a backup (NAME, else the newest one -
                                 of version V when given) into DIR and check
                                 its checksums; prints the name of the dump

Environment: BACKUP_S3_BUCKET, BACKUP_S3_ENDPOINT, BACKUP_S3_REGION,
BACKUP_S3_PREFIX, BACKUP_S3_ACCESS_KEY_ID, BACKUP_S3_SECRET_ACCESS_KEY,
BACKUP_S3_PATH_STYLE, BACKUP_S3_KEEP; the keys may come from files instead:
BACKUP_S3_ACCESS_KEY_ID_FILE, BACKUP_S3_SECRET_ACCESS_KEY_FILE (a path
inside the container, readable by the user it runs as).

Exit status: 0 - done, 1 - failed, 2 - wrong arguments, 3 - get found no
backup.`

// backupS3 — `/api backup-s3 put|list|get` (CLAB-132). Образ distroless, и
// выгрузку, как healthcheck и dbcheck, делает сам бинарник.
func backupS3(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, backupS3Usage)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("backup-s3 "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, backupS3Usage) }
	partSize := fs.Int64("part-size", blobstore.DefaultPartSize, "multipart part size in bytes, 5 MiB or more")
	dir := fs.String("dir", ".", "where get writes the files")
	version := fs.String("version", "", "get: the newest backup of this version")
	switch cmd {
	case "put", "list", "get":
	default:
		fmt.Fprintf(stderr, "✗ unknown command %q of backup-s3\n\n%s\n", cmd, backupS3Usage)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, ok, err := offsite.FromEnv(os.Getenv)
	switch {
	case !ok:
		fmt.Fprintln(stderr, "✗ BACKUP_S3_BUCKET is not set: there is no bucket for the off-machine copy")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	store, err := offsite.Open(cfg, blobstore.S3Config{})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	store.PartSize = *partSize

	switch cmd {
	case "put":
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "✗ put takes one dump: /api backup-s3 put /backups/cadence-vX.Y.Z-<date>.sql.gz")
			return 2
		}
		return backupS3Put(ctx, store, fs.Arg(0), stdout, stderr)
	case "list":
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "✗ list takes no arguments")
			return 2
		}
		return backupS3List(ctx, store, stdout, stderr)
	default:
		if fs.NArg() > 1 {
			fmt.Fprintln(stderr, "✗ get takes one backup name at a time")
			return 2
		}
		return backupS3Get(ctx, store, fs.Arg(0), *version, *dir, stdout, stderr)
	}
}

func backupS3Put(ctx context.Context, store *offsite.Store, dump string, stdout, stderr io.Writer) int {
	res, err := store.Upload(ctx, dump)
	if len(res.Files) > 0 && strings.HasSuffix(res.Files[len(res.Files)-1], offsite.SumSuffix) {
		fmt.Fprintf(stdout, "✓ uploaded %s to %s (%s)\n", strings.Join(res.Files, ", "), store.Location(), humanSize(res.Bytes))
	}
	if err != nil {
		fmt.Fprintf(stderr, "✗ upload to %s failed: %v\n", store.Location(), err)
		return 1
	}
	line := fmt.Sprintf("✓ %s keeps %d of BACKUP_S3_KEEP=%d backups", store.Location(), res.Kept, store.Keep())
	if len(res.Removed) > 0 {
		line += "; removed " + strings.Join(res.Removed, ", ")
	}
	fmt.Fprintln(stdout, line)
	return 0
}

func backupS3List(ctx context.Context, store *offsite.Store, stdout, stderr io.Writer) int {
	backups, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ listing %s failed: %v\n", store.Location(), err)
		return 1
	}
	if len(backups) == 0 {
		fmt.Fprintf(stdout, "no backups in %s\n", store.Location())
		return 0
	}
	for _, b := range backups {
		state := "complete"
		if !b.Complete() {
			state = "incomplete"
		}
		blobs := ""
		if _, ok := b.Objects[offsite.BlobsSuffix]; ok {
			blobs = " +blobs"
		}
		fmt.Fprintf(stdout, "%s  %6s  %s%s\n", b.Stem, humanSize(b.Size()), state, blobs)
	}
	return 0
}

func backupS3Get(ctx context.Context, store *offsite.Store, name, version, dir string, stdout, stderr io.Writer) int {
	b, err := store.Pick(ctx, name, version)
	switch {
	case errors.Is(err, offsite.ErrNone):
		switch {
		case name != "":
			fmt.Fprintf(stderr, "✗ no backup %s in %s\n", strings.TrimSuffix(name, offsite.DumpSuffix), store.Location())
		case version != "":
			fmt.Fprintf(stderr, "✗ no complete backup of %s in %s\n", version, store.Location())
		default:
			fmt.Fprintf(stderr, "✗ no complete backup in %s\n", store.Location())
		}
		return 3
	case err != nil:
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "▸ fetching %s from %s\n", b.Stem, store.Location())
	got, err := store.Fetch(ctx, b, dir)
	if err != nil {
		fmt.Fprintf(stderr, "✗ fetching %s failed: %v\n", b.Stem, err)
		return 1
	}
	files := b.Stem + offsite.DumpSuffix
	if got.Blobs != "" {
		files += ", " + b.Stem + offsite.BlobsSuffix
	}
	fmt.Fprintf(stderr, "✓ fetched %s (%s): the checksums match\n", files, humanSize(got.Bytes))
	fmt.Fprintln(stdout, b.Stem+offsite.DumpSuffix)
	return 0
}

// humanSize — размер как du -h у скриптов: 512, 12K, 3.4M.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d", n)
	}
	value, suffix := float64(n), ""
	for _, s := range []string{"K", "M", "G", "T"} {
		value /= unit
		suffix = s
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, suffix)
	}
	return fmt.Sprintf("%.0f%s", value, suffix)
}
