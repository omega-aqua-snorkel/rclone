// Package create implements 'rclone archive create'.
package create

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mholt/archives"
	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/cmd/archive/files"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"
)

// Globals
var (
	archiveFormats = map[string]archives.CompressedArchive{
		"tar": archives.CompressedArchive{
			Archival: archives.Tar{},
		},
		"tar.gz": archives.CompressedArchive{
			Compression: archives.Gz{},
			Archival:    archives.Tar{},
		},
		"tar.bz2": archives.CompressedArchive{
			Compression: archives.Bz2{},
			Archival:    archives.Tar{},
		},
		"tar.lz": archives.CompressedArchive{
			Compression: archives.Lzip{},
			Archival:    archives.Tar{},
		},
		"tar.lz4": archives.CompressedArchive{
			Compression: archives.Lz4{},
			Archival:    archives.Tar{},
		},
		"tar.xz": archives.CompressedArchive{
			Compression: archives.Xz{},
			Archival:    archives.Tar{},
		},
		"tar.zst": archives.CompressedArchive{
			Compression: archives.Zstd{},
			Archival:    archives.Tar{},
		},
		"tar.br": archives.CompressedArchive{
			Compression: archives.Brotli{},
			Archival:    archives.Tar{},
		},
		"tar.sz": archives.CompressedArchive{
			Compression: archives.Sz{},
			Archival:    archives.Tar{},
		},
		"zip": archives.CompressedArchive{
			Archival: archives.Zip{},
		},
	}
	archiveExtensions = map[string]string{
		// zip
		"*.zip": "zip",
		// tar
		"*.tar": "tar",
		// tar.gz
		"*.tar.gz": "tar.gz",
		"*.tgz":    "tar.gz",
		"*.taz":    "tar.gz",
		// tar.bz2
		"*.tar.bz2": "tar.bz2",
		"*.tb2":     "tar.bz2",
		"*.tbz":     "tar.bz2",
		"*.tbz2":    "tar.bz2",
		"*.tz2":     "tar.bz2",
		// tar.lz
		"*.tar.lz": "tar.lz",
		// tar.xz
		"*.tar.xz": "tar.xz",
		"*.txz":    "tar.xz",
		// tar.zst
		"*.tar.zst": "tar.zst",
		"*.tzst":    "tar.zst",
		// tar.br
		"*.tar.br": "tar.br",
		// tar.sz
		"*.tar.sz": "tar.sz",
	}
	filesAdded = 0
)

type archivesFileInfoList []archives.FileInfo

func (a archivesFileInfoList) Len() int {
	return len(a)
}

func (a archivesFileInfoList) Less(i, j int) bool {
	if a[i].FileInfo.IsDir() == a[j].FileInfo.IsDir() {
		// both are same type, order by name
		return strings.Compare(a[i].NameInArchive, a[j].NameInArchive) < 0
	} else if a[i].FileInfo.IsDir() {
		return strings.Compare(strings.TrimSuffix(a[i].NameInArchive, "/"), path.Dir(a[j].NameInArchive)) < 0
	}
	return strings.Compare(path.Dir(a[i].NameInArchive), strings.TrimSuffix(a[j].NameInArchive, "/")) < 0
}

func (a archivesFileInfoList) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func init() {
}

func getCompressor(format string, filename string) (archives.CompressedArchive, error) {
	var compressor archives.CompressedArchive
	var found bool
	// make filename lowercase for checks
	filename = strings.ToLower(filename)
	//
	if format == "" {
		// format flag not set, get format from the file extension
		for pattern, formatName := range archiveExtensions {
			ok, err := path.Match(pattern, filename)
			if err != nil {
				// error in pattern
				return archives.CompressedArchive{}, fmt.Errorf("invalid extension pattern '%s'", pattern)
			} else if ok {
				// pattern matches filename, get compressor
				compressor, found = archiveFormats[formatName]
				break
			}
		}
	} else {
		// format flag set, look for it
		compressor, found = archiveFormats[format]
	}
	//
	if found {
		return compressor, nil
	} else if format == "" {
		return archives.CompressedArchive{}, fmt.Errorf("format not set and can't be guessed from extension")
	}
	return archives.CompressedArchive{}, fmt.Errorf("invalid format '%s'", format)
}

func getRemoteFromFs(src fs.Fs, dstFile string) string {
	if src.Features().IsLocal {
		return path.Join(src.Root(), dstFile)
	}
	return fmt.Sprintf("%s:%s", src.Name(), path.Join(src.Root(), dstFile))
}

// CheckValidDestination - takes fs.Fs and dstFile and checks if directory is valid
func CheckValidDestination(ctx context.Context, dst fs.Fs, dstFile string) (fs.Fs, string, error) {
	var err error
	// check if dst + dstFile is a file
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		// dst is a valid directory, dstFile is a valid file
		// we are overwriting the file, all is well
		fs.Debugf(nil, "%s valid (file exist)\n", getRemoteFromFs(dst, dstFile))
		return dst, dstFile, nil
	} else if errors.Is(err, fs.ErrorIsDir) {
		// dst is a directory
		// we need a file name, not good
		fs.Debugf(nil, "%s invalid\n", getRemoteFromFs(dst, dstFile))
		return dst, dstFile, fmt.Errorf("%s %w", getRemoteFromFs(dst, dstFile), err)
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		// dst is a directory (we need a filename) or some other error happened
		// not good, leave
		fs.Debugf(nil, "%s invalid - %v\n", getRemoteFromFs(dst, dstFile), err)
		return dst, "", fmt.Errorf("%s is invalid: %w", getRemoteFromFs(dst, dstFile), err)
	}
	// if we are here dst points to a non existing path
	// we must check if parent is a valid directory
	fs.Debugf(nil, "%s does not exist, check if parent is a valid directory\n", getRemoteFromFs(dst, dstFile))
	parentDir, parentFile := path.Split(getRemoteFromFs(dst, dstFile))
	dst, dstFile = cmd.NewFsFile(parentDir)
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		// parent is a file
		// we cant use this, not good
		fs.Debugf(nil, "%s invalid - parent is a file\n", getRemoteFromFs(dst, dstFile))
		return dst, parentFile, fmt.Errorf("can't create %s, %s is a file", parentFile, parentDir)
	} else if errors.Is(err, fs.ErrorIsDir) {
		// parent is a directory
		// file does not exist, we are creating is, all is good
		fs.Debugf(nil, "%s valid - parent is a dir, file does not exist\n", getRemoteFromFs(dst, dstFile))
		return dst, parentFile, nil
	}
	// something else happened
	fs.Debugf(nil, "%s invalid - %v\n", getRemoteFromFs(dst, dstFile), err)
	return dst, parentFile, fmt.Errorf("invalid parent dir %s: %w", parentDir, err)
}

func onProgress(snapshot accounting.TransferSnapshot, action int) {
	if action == files.Closing {
		fs.Printf(nil, "Add %s", snapshot.Name)
		filesAdded++
	}
}

func loadMetadata(ctx context.Context, o fs.DirEntry) fs.Metadata {
	meta, err := fs.GetMetadata(ctx, o)
	if err != nil {
		meta = make(fs.Metadata, 0)
	}
	return meta
}

// ArchiveCreate - compresses/archive source to destination
func ArchiveCreate(ctx context.Context, src fs.Fs, dst fs.Fs, dstFile string, format string, prefix string) error {
	var err error
	var list archivesFileInfoList
	var compArchive archives.CompressedArchive
	// check id dst is valid
	if dst != nil {
		dst, dstFile, err = CheckValidDestination(ctx, dst, dstFile)
		if err != nil {
			return err
		}
	}
	//
	fi := filter.GetConfig(ctx)
	// get archive format
	compArchive, err = getCompressor(format, dstFile)
	if err != nil {
		return err
	}
	// get source files
	err = walk.Walk(ctx, src, "", false, -1, func(path string, entries fs.DirEntries, err error) error {
		// get directories
		entries.ForDir(func(o fs.Directory) {
			if fi.Include(o.Remote(), o.Size(), o.ModTime(ctx), loadMetadata(ctx, o)) {
				info := files.NewArchiveFileInfo(ctx, o, prefix, onProgress)
				list = append(list, info)
			}
		})
		// get files
		entries.ForObject(func(o fs.Object) {
			if fi.Include(o.Remote(), o.Size(), o.ModTime(ctx), loadMetadata(ctx, o)) {
				info := files.NewArchiveFileInfo(ctx, o, prefix, onProgress)
				list = append(list, info)
			}
		})
		return nil
	})
	if err != nil {
		return err
	} else if list.Len() == 0 {
		return fmt.Errorf("no files found in source")
	}
	sort.Stable(list)
	// create destination
	if dst != nil {
		// create io.Pipe
		pipeReader, pipeWriter := io.Pipe()
		// write to pipewriter in background
		go func() {
			err := compArchive.Archive(ctx, pipeWriter, list)
			pipeWriter.CloseWithError(err)
		}()
		// rcat to remote from pipereader
		_, err = operations.Rcat(ctx, dst, dstFile, pipeReader, time.Now(), nil)
		fs.Printf(nil, "Total files added %d", filesAdded)
		return err
	}
	// write to stdout
	err = compArchive.Archive(ctx, os.Stdout, list)
	fs.Printf(nil, "Total files added %d", filesAdded)
	return err
}
