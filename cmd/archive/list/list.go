// Package list inplements 'rclone archive list'
package list

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mholt/archives"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"

	"github.com/rclone/rclone/cmd/archive/files"
)

// ArchiveList -- print a list of the files in the archive
func ArchiveList(ctx context.Context, src fs.Fs, srcFile string, longList bool) error {
	var srcObj fs.Object
	var err error
	//
	ci := fs.GetConfig(ctx)
	fi := filter.GetConfig(ctx)
	// get object
	srcObj, err = src.NewObject(ctx, srcFile)
	if err != nil {
		return fmt.Errorf("source is not a file, %w", err)
	}
	// open source, removed the accounting code because extract
	// needs a SeekableFile (io.Seeker/io.ReadAt) for zip/7z
	/*

		tr := accounting.Stats(ctx).NewTransfer(srcObj, nil)
		defer tr.Done(ctx, err)
		//
		var options []fs.OpenOption
		for _, option := range fs.GetConfig(ctx).DownloadHeaders {
			options = append(options, option)
		}
		var in io.Reader
		in, err = operations.Open(ctx, srcObj, options...)
	*/

	in, err := files.NewSeekableFile(ctx, srcObj, 5)

	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", srcFile, err)
	}
	// identify format
	format, _, err := archives.Identify(ctx, "", in)
	//
	if err != nil {
		return fmt.Errorf("failed to open check file type, %w", err)
	}
	fs.Debugf(src, "Listing %s/%s, format %s", src.Root(), srcFile, strings.TrimPrefix(format.Extension(), "."))
	// check if extract is supported by format
	ex, isExtract := format.(archives.Extraction)
	if !isExtract {
		return fmt.Errorf("extraction for %s not supported", strings.TrimPrefix(format.Extension(), "."))
	}
	// list files
	err = ex.Extract(ctx, in, func(ctx context.Context, f archives.FileInfo) error {
		// check if excluded
		if !fi.Include(f.NameInArchive, f.Size(), f.ModTime(), fs.Metadata{}) {
			return nil
		}
		// get entry name 
		name:=f.NameInArchive
		if f.IsDir() && !strings.HasSuffix(name,"/") { name = name+"/" }
		// print info
		if longList {
			operations.SyncFprintf(os.Stdout, "%s %s %s\n", operations.SizeStringField(f.Size(), ci.HumanReadable, 9), f.ModTime().Format("2006-01-02 15:04:05.000000000"), name)
		} else {
			operations.SyncFprintf(os.Stdout, "%s %s\n", operations.SizeStringField(f.Size(), ci.HumanReadable, 9), name)
		}
		return nil
	})
	//
	return err
}
