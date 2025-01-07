package list

import (
	"context"
	"io"
	"os"
	"fmt"
	"strings"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
        "github.com/rclone/rclone/fs/config/flags"
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
        cmdFlags := Command.Flags()
        flags.BoolVarP(cmdFlags, &long_list, "long", "", long_list, "List extra attributtes", "")
}

func ListArchive(ctx context.Context, src fs.Fs,srcFile string) error {
	var srcObj fs.Object
	var err error
	//
	ci := fs.GetConfig(ctx)
	fi := filter.GetConfig(ctx)
	// get object
	srcObj, err = src.NewObject(ctx, srcFile)
	if err != nil {
		return fmt.Errorf("Error opening file, %v",err)
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
		fs.Debugf(src,"Listing %s/%s, format %s",src.Root(),srcFile, strings.TrimPrefix(format.Extension(),"."))
	}
	// check if extract is supported by format
	ex, isExtract := format.(archives.Extraction)
	if !isExtract {
		return fmt.Errorf("Extraction for %s not supported", strings.TrimPrefix(format.Extension(),"."))
	}
	// list files
	err = ex.Extract(ctx, in, func(ctx context.Context, f archives.FileInfo) error {
		if ! fi.Include(f.NameInArchive,f.Size(),f.ModTime(),fs.Metadata{}) {
			return nil
		}else if long_list {
			operations.SyncFprintf(os.Stdout, "%s %s %s\n", operations.SizeStringField(f.Size(), ci.HumanReadable, 9), f.ModTime().Format("2006-01-02 15:04:05.000000000"), f.NameInArchive)
		}else{
			operations.SyncFprintf(os.Stdout, "%s %s\n", operations.SizeStringField(f.Size(), ci.HumanReadable, 9), f.NameInArchive)
		}
		return nil
	})
	//
	return err
}



var Command = &cobra.Command{
	Use:   "list [flags] <source>",
	Short: `List archive contents from source.`,
	// Warning! "|" will be replaced by backticks below
	Long: strings.ReplaceAll(`List contents of an archive to the console, will autodetect format
`, "|", "`"),
	Annotations: map[string]string{
		"versionIntroduced": "v1.68",
		"groups":            "Copy,Filter,Listing",
	},
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(1, 1, command, args)
		//
		src,srcFile:=cmd.NewFsFile(args[0])
		//
		cmd.Run(false, false, command, func() error {
			return ListArchive(context.Background(), src,srcFile)
		})

	},
}
