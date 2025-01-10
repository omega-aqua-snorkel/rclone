// Package extract implements 'rclone archive extract'
package extract

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/mholt/archives"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/cmd/archive/files"

)

func init() {
}

// ExtractArchive -- extracts files from source to destination
func ExtractArchive(ctx context.Context, src fs.Fs, srcFile string, dst fs.Fs, dstFile string) error {
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
	fs.Debugf(src, "Source archive file: %s/%s", src.Root(), srcFile)
	// get dst object
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		return fmt.Errorf("destination can't be a file")
	} else if errors.Is(err, fs.ErrorObjectNotFound) {
		return fmt.Errorf("destination not found")
	} else if !errors.Is(err, fs.ErrorIsDir) {
		return fmt.Errorf("unable to access destination, %w", err)
	}
	// clear error, previous ckeck shoud end with err==fs.ErrorIsDir
	fs.Debugf(dst, "Destination for extracted files: %s", dst.Root())
	/*
		// open source
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
	fs.Debugf(src, "Extract %s/%s, format %s to %s", src.Root(), srcFile, strings.TrimPrefix(format.Extension(), "."), dst.Root())

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
		} else if f.IsDir() {
			return nil
		}
		// create directory if needed
		dir, _ := path.Split(f.NameInArchive)
		if dir != "" {
			err := operations.Mkdir(ctx, dst, dir)
			if err != nil {
				return err
			}
		}
		// open the file
		fin, err := f.Open()
		if err != nil {
			return err
		}
		// extract the file to destination
		_, err = operations.Rcat(ctx, dst, f.NameInArchive, fin, f.ModTime(), nil)
		if err == nil {
			fs.Infof(src, "extracted %s\n", f.NameInArchive)
		}
		//
		return err
	})
	//
	return err
}
