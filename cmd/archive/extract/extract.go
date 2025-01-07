package extract

import (
	"context"
	"os"
	"io"
	"fmt"
	"strings"
	"errors"
	"path"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
        "github.com/rclone/rclone/fs/filter"
  	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/mholt/archives"
	"github.com/spf13/cobra"
)


// Globals
var (
        long_list = bool(false)
)

func init() {
}

func ExtractArchive(ctx context.Context, src fs.Fs,srcFile string,dst fs.Fs,dstFile string) error {
	var srcObj fs.Object
	var err error
	//
	fi := filter.GetConfig(ctx)
	// get source object
	srcObj, err = src.NewObject(ctx, srcFile)
        if errors.Is(err, fs.ErrorIsDir) {
		return fmt.Errorf("Source can't be a directory")
        } else if errors.Is(err, fs.ErrorObjectNotFound) {
		return fmt.Errorf("Source not found")
	} else if err!=nil {
		return fmt.Errorf("Unable to access source, %v",err)
        } else {
		fs.Debugf(src,"Source archive file: %s/%s",src.Root(),srcFile)
	}
	// get dst object
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		return fmt.Errorf("Destination can't be a file")
	} else if errors.Is(err, fs.ErrorObjectNotFound) {
		return fmt.Errorf("Destination not found")
	} else if !errors.Is(err, fs.ErrorIsDir) {
		return fmt.Errorf("Unable to access destination, %v",err)
        } else {
		err=nil
		fs.Debugf(dst,"Destination for extracted files: %s",dst.Root())
	}
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
	if err != nil {
		return fmt.Errorf("Failed to open file %s: %w",srcFile,err)
	}
	// identify format
	format,in,err := archives.Identify(ctx, "", in)
	//
	if err != nil {
		return fmt.Errorf("Failed to open check file type, %v",err)
	} else {
		fs.Debugf(src,"Extract %s/%s, format %s to %s",src.Root(),srcFile, strings.TrimPrefix(format.Extension(),"."),dst.Root())
	}
	// check if extract is supported by format
	ex, isExtract := format.(archives.Extraction)
	if !isExtract {
		return fmt.Errorf("Extraction for %s not supported", strings.TrimPrefix(format.Extension(),"."))
	}
	// extract files
	err = ex.Extract(ctx, in, func(ctx context.Context, f archives.FileInfo) error {
		// check if file should be extracted
		if ! fi.Include(f.NameInArchive,f.Size(),f.ModTime(),fs.Metadata{}) {
			return nil
		}
		// create directory if needed
		dir,_:=path.Split(f.NameInArchive)
		if dir != "" {
			operations.Mkdir(ctx,dst,dir)
		}
		// open the file
		fin,err := f.Open()
		if err != nil { return err }
		// extract the file to destination
		_,err = operations.Rcat(ctx, dst,f.NameInArchive,fin, f.ModTime(), nil)
		if err == nil {
			operations.SyncFprintf(os.Stdout, "x %s\n", f.NameInArchive)
		}
		//
		return err
	})
	//
	return err
}



var Command = &cobra.Command{
	Use:   "extract [flags] <source> <destination>",
	Short: `Extract archives from source to destination.`,
	// Warning! "|" will be replaced by backticks below
	Long: strings.ReplaceAll(`Extract archive contents to destination directory, will autodetect format
`, "|", "`"),
	Annotations: map[string]string{
		"versionIntroduced": "v1.68",
		"groups":            "Copy,Filter,Listing",
	},
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(2, 2, command, args)
		//
		src,srcFile:=cmd.NewFsFile(args[0])
		dst,dstFile:=cmd.NewFsFile(args[1])
		//
		cmd.Run(false, false, command, func() error {
			return ExtractArchive(context.Background(), src,srcFile,dst,dstFile)
		})

	},
}
