package filesystem

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/juju/ratelimit"
	"github.com/karrick/godirwalk"
	"github.com/klauspost/pgzip"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/system"
)

const memory = 4 * 1024

var pool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, memory)
		return b
	},
}

// Progress is used to track the progress of any I/O operation that are being
// performed.
type Progress struct {
	// written is the total size of the files that have been written to the writer.
	written int64
	// Total is the total size of the archive in bytes.
	total int64
	// w .
	w io.Writer
}

// NewProgress .
func NewProgress(total int64) *Progress {
	return &Progress{total: total}
}

// Written returns the total number of bytes written.
// This function should be used when the progress is tracking data being written.
func (p *Progress) Written() int64 {
	return atomic.LoadInt64(&p.written)
}

// Total returns the total size in bytes.
func (p *Progress) Total() int64 {
	return atomic.LoadInt64(&p.total)
}

// Write totals the number of bytes that have been written to the writer.
func (p *Progress) Write(v []byte) (int, error) {
	n := len(v)
	atomic.AddInt64(&p.written, int64(n))
	if p.w != nil {
		return p.w.Write(v)
	}
	return n, nil
}

// Progress returns a formatted progress string for the current progress.
func (p *Progress) Progress(width int) string {
	current := p.Written()
	total := p.Total()

	// v = 100 (Progress)
	// size = 1000 (Content-Length)
	// p / size = 0.1
	// * 100 = 10% (Multiply by 100 to get a percentage of the download)
	// 10% / tickPercentage = (10% / (100 / 25)) (Divide by tick percentage to get the number of ticks)
	// 2.5 (Number of ticks as a float64)
	// 2 (convert to an integer)

	// We have to cast these numbers to float in order to get a float result from the division.
	ticks := ((float64(current) / float64(total)) * 100) / (float64(100) / float64(width))
	bar := strings.Repeat("=", int(ticks)) + strings.Repeat(" ", width-int(ticks))
	return "[" + bar + "] " + system.FormatBytes(current) + " / " + system.FormatBytes(total)
}

type Archive struct {
	// BasePath is the absolute path to create the archive from where Files and Ignore are
	// relative to.
	BasePath string

	// Ignore is a gitignore string (most likely read from a file) of files to ignore
	// from the archive.
	Ignore string

	// Files specifies the files to archive, this takes priority over the Ignore option, if
	// unspecified, all files in the BasePath will be archived unless Ignore is set.
	Files []string

	// Progress wraps the writer of the archive to pass through the progress tracker.
	Progress *Progress
}

// Create creates an archive at dst with all the files defined in the
// included Files array.
func (a *Archive) Create(dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Select a writer based off of the WriteLimit configuration option. If there is no
	// write limit, use the file as the writer.
	var writer io.Writer
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		// Token bucket with a capacity of "writeLimit" MiB, adding "writeLimit" MiB/s
		// and then wrap the file writer with the token bucket limiter.
		writer = ratelimit.Writer(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	} else {
		writer = f
	}

	// Choose which compression level to use based on the compression_level configuration option
	var compressionLevel int
	switch config.Get().System.Backups.CompressionLevel {
	case "none":
		compressionLevel = pgzip.NoCompression
	case "best_compression":
		compressionLevel = pgzip.BestCompression
	case "best_speed":
		fallthrough
	default:
		compressionLevel = pgzip.BestSpeed
	}

	// Create a new gzip writer around the file.
	gw, _ := pgzip.NewWriterLevel(writer, compressionLevel)
	_ = gw.SetConcurrency(1<<20, 1)
	defer gw.Close()

	var pw io.Writer
	if a.Progress != nil {
		a.Progress.w = gw
		pw = a.Progress
	} else {
		pw = gw
	}

	// Create a new tar writer around the gzip writer.
	tw := tar.NewWriter(pw)
	defer tw.Close()

	// Configure godirwalk.
	options := &godirwalk.Options{
		FollowSymbolicLinks: false,
		Unsorted:            true,
		Callback:            a.callback(tw),
	}

	// If we're specifically looking for only certain files, or have requested
	// that certain files be ignored we'll update the callback function to reflect
	// that request.
	if len(a.Files) == 0 && len(a.Ignore) > 0 {
		i := ignore.CompileIgnoreLines(strings.Split(a.Ignore, "\n")...)

		options.Callback = a.callback(tw, func(_ string, rp string) error {
			if i.MatchesPath(rp) {
				return godirwalk.SkipThis
			}

			return nil
		})
	} else if len(a.Files) > 0 {
		options.Callback = a.withFilesCallback(tw)
	}

	// Recursively walk the path we are archiving.
	return godirwalk.Walk(a.BasePath, options)
}

// Callback function used to determine if a given file should be included in the archive
// being generated.
func (a *Archive) callback(tw *tar.Writer, opts ...func(path string, relative string) error) func(path string, de *godirwalk.Dirent) error {
	return func(path string, de *godirwalk.Dirent) error {
		// Skip directories because we are walking them recursively.
		if de.IsDir() {
			return nil
		}

		relative := filepath.ToSlash(strings.TrimPrefix(path, a.BasePath+string(filepath.Separator)))

		// Call the additional options passed to this callback function. If any of them return
		// a non-nil error we will exit immediately.
		for _, opt := range opts {
			if err := opt(path, relative); err != nil {
				return err
			}
		}

		// Add the file to the archive, if it is nested in a directory,
		// the directory will be automatically "created" in the archive.
		return a.addToArchive(path, relative, tw)
	}
}

// Pushes only files defined in the Files key to the final archive.
func (a *Archive) withFilesCallback(tw *tar.Writer) func(path string, de *godirwalk.Dirent) error {
	return a.callback(tw, func(p string, rp string) error {
		for _, f := range a.Files {
			// If the given doesn't match, or doesn't have the same prefix continue
			// to the next item in the loop.
			if p != f && !strings.HasPrefix(strings.TrimSuffix(p, "/")+"/", f) {
				continue
			}

			// Once we have a match return a nil value here so that the loop stops and the
			// call to this function will correctly include the file in the archive. If there
			// are no matches we'll never make it to this line, and the final error returned
			// will be the godirwalk.SkipThis error.
			return nil
		}

		return godirwalk.SkipThis
	})
}

// Adds a given file path to the final archive being created.
func (a *Archive) addToArchive(p string, rp string, w *tar.Writer) error {
	// Lstat the file, this will give us the same information as Stat except that it will not
	// follow a symlink to its target automatically. This is important to avoid including
	// files that exist outside the server root unintentionally in the backup.
	s, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed executing os.Lstat on '%s'", rp)
	}

	// Skip socket files as they are unsupported by archive/tar.
	// Error will come from tar#FileInfoHeader: "archive/tar: sockets not supported"
	if s.Mode()&fs.ModeSocket != 0 {
		return nil
	}

	// Resolve the symlink target if the file is a symlink.
	var target string
	if s.Mode()&fs.ModeSymlink != 0 {
		// Read the target of the symlink. If there are any errors we will dump them out to
		// the logs, but we're not going to stop the backup. There are far too many cases of
		// symlinks causing all sorts of unnecessary pain in this process. Sucks to suck if
		// it doesn't work.
		target, err = os.Readlink(s.Name())
		if err != nil {
			// Ignore the not exist errors specifically, since theres nothing important about that.
			if !os.IsNotExist(err) {
				log.WithField("path", rp).WithField("readlink_err", err.Error()).Warn("failed reading symlink for target path; skipping...")
			}
			return nil
		}
	}

	// Get the tar FileInfoHeader in order to add the file to the archive.
	header, err := tar.FileInfoHeader(s, filepath.ToSlash(target))
	if err != nil {
		return errors.WrapIff(err, "failed to get tar#FileInfoHeader for '%s'", rp)
	}

	// Fix the header name if the file is not a symlink.
	if s.Mode()&fs.ModeSymlink == 0 {
		header.Name = rp
	}

	// Write the tar FileInfoHeader to the archive.
	if err := w.WriteHeader(header); err != nil {
		return errors.WrapIff(err, "failed to write tar#FileInfoHeader for '%s'", rp)
	}

	// If the size of the file is less than 1 (most likely for symlinks), skip writing the file.
	if header.Size < 1 {
		return nil
	}

	// If the buffer size is larger than the file size, create a smaller buffer to hold the file.
	var buf []byte
	if header.Size < memory {
		buf = make([]byte, header.Size)
	} else {
		// Get a fixed-size buffer from the pool to save on allocations.
		buf = pool.Get().([]byte)
		defer func() {
			buf = make([]byte, memory)
			pool.Put(buf)
		}()
	}

	// Open the file.
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.WrapIff(err, "failed to open '%s' for copying", header.Name)
	}
	defer f.Close()

	// Copy the file's contents to the archive using our buffer.
	if _, err := io.CopyBuffer(w, io.LimitReader(f, header.Size), buf); err != nil {
		return errors.WrapIff(err, "failed to copy '%s' to archive", header.Name)
	}

	return nil
}
