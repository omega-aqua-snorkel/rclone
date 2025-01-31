// Package extract implements 'rclone archive extract'
package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mholt/archives"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
)

func init() {
}

// ArchiveExtract -- extracts files from source to destination
func ArchiveExtract(ctx context.Context, src fs.Fs, srcFile string, dst fs.Fs, dstFile string) error {
	var srcObj fs.Object
	var err error
	//
	fi := filter.GetConfig(ctx)
	// get source object
	srcObj, err = src.NewObject(ctx, srcFile)
	if errors.Is(err, fs.ErrorIsDir) {
		return fmt.Errorf("source can't be a directory")
	} else if errors.Is(err, fs.ErrorObjectNotFound) {
		return fmt.Errorf("source not found")
	} else if err != nil {
		return fmt.Errorf("unable to access source, %w", err)
	}
	fs.Debugf(nil,"Source archive file: %s/%s", src.Root(), srcFile)
	// get dst object
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		return fmt.Errorf("destination can't be a file")
	} else if errors.Is(err, fs.ErrorObjectNotFound) {
		return fmt.Errorf("destination not found")
	} else if !errors.Is(err, fs.ErrorIsDir) {
		return fmt.Errorf("unable to access destination, %w", err)
	}
	//
	err = nil
	fs.Debugf(nil,"Destination for extracted files: %s", dst.Root())
	// start accounting
	tr := accounting.Stats(ctx).NewTransfer(srcObj, nil)
	defer tr.Done(ctx, err)
	// open source
	var options []fs.OpenOption
	for _, option := range fs.GetConfig(ctx).DownloadHeaders {
		options = append(options, option)
	}
	var in io.ReadCloser
	in, err = operations.Open(ctx, srcObj, options...)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", srcFile, err)
	}
	// account and buffer the transfer
	in = tr.Account(ctx, in).WithBuffer()
	// identify format
	format, _, err := archives.Identify(ctx, "", in)
	//
	if err != nil {
		return fmt.Errorf("failed to open check file type, %w", err)
	}
	fs.Debugf(nil,"Extract %s/%s, format %s to %s", src.Root(), srcFile, strings.TrimPrefix(format.Extension(), "."), dst.Root())

	// check if extract is supported by format
	ex, isExtract := format.(archives.Extraction)
	if !isExtract {
		return fmt.Errorf("extraction for %s not supported", strings.TrimPrefix(format.Extension(), "."))
	}
	// extract files
	err = ex.Extract(ctx, in, func(ctx context.Context, f archives.FileInfo) error {
		// check if file should be extracted
		if !fi.Include(f.NameInArchive, f.Size(), f.ModTime(), fs.Metadata{}) {
			return nil
		}
		//
		if f.IsDir() {
			// directory, try and crerate it
			err := operations.Mkdir(ctx, dst, f.NameInArchive)
			if err == nil {
				fs.Debugf(nil,"mkdir %s\n", f.NameInArchive)
			}
		} else {
			// file, open it
			fin, err := f.Open()
			if err != nil {
				return err
			}
			// extract the file to destination
			_, err = operations.Rcat(ctx, dst, f.NameInArchive, fin, f.ModTime(), nil)
			if err == nil {
				fs.Infof(nil,"extract %s\n", f.NameInArchive)
			}
		}
		return err
	})
	//
	return err
}
