// Package cat provides the cat command.
package archive

import (
	"context"
	"io"
	"os"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sort"
	"errors"
	"time"
	"regexp"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/walk"
	"github.com/mholt/archives"
	"github.com/spf13/cobra"
)

// Globals
var (
	fullpath = bool(false)
	format = string("")
)

// RCloneMetadata

func init() {
	cmd.Root.AddCommand(commandDefinition)
	cmdFlags := commandDefinition.Flags()
	flags.BoolVarP(cmdFlags, &fullpath, "fullpath", "", fullpath, "Save full path in archive", "")
	flags.StringVarP(cmdFlags, &format, "format", "",format, "Compress the archive using the selected format. If not set will try and guess from extension. Valid formats: zip,tar,tar.gz,tar.bz2,tar.lz,tar.lz4,tar.xz,tar.zst,tar.br,tar.sz","")
}

func NameMatches(pattern string,name string) bool {
	ok, _ := regexp.MatchString(pattern, name)
	return ok
}

func GetFormatFromFile(filename string) string {
	// make filename lowercase for checks
	filename=strings.ToLower(filename)
	// I think regex is better to check file extensions
	if format != "" { 
		// format flag set, use it
		return format
	}else if NameMatches("\\.(zip)$",filename) {
		return "zip"
	}else if NameMatches("\\.(tar)$",filename) {
		return "tar"
	}else if NameMatches("\\.(tar.gz|tgz|taz)$",filename) {
		return "tar.gz"
	}else if NameMatches("\\.(tar.bz2|tb2|tbz|tbz2|tz2)$",filename) {
		return "tar.bz2"
	}else if NameMatches("\\.(tar.lz)$",filename) {
		return "tar.lz"
	}else if NameMatches("\\.(tar.xz|txz)$",filename) {
		return "tar.xz"
	}else if NameMatches("\\.(tar.zst|tzst)$",filename) {
		return "tar.zst"
	}else if NameMatches("\\.(tar.br)$",filename) {
		return "tar.br"
	}else if NameMatches("\\.(tar.sz)$",filename) {
		return "tar.sz"
	} else {
		return ""
	}
}

func DirEntryToFileInfo(ctx context.Context,src fs.Fs,entry fs.Object,printMsg bool) archives.FileInfo {
	// get entry type
	dirType := reflect.TypeOf((*fs.Directory)(nil)).Elem()
	// fill structure
       	name := entry.Remote()
        size := entry.Size()
        mtime := entry.ModTime(ctx)
        isDir := reflect.TypeOf(entry).Implements(dirType)
	if fullpath { name=path.Join(strings.TrimPrefix(src.Root(),"/"),name)  }
	// get entry metadata, not used right now
	// metadata,_ := fs.GetMetadata(ctx, entry)
	//
	return NewArchivesFileInfo(name,size,mtime,isDir,func(fi FileInfoFS)(RCloneFile,error){
		var err error
		// I don't know if this is needed, just copied the open file from cat command
		tr := accounting.Stats(ctx).NewTransfer(entry, nil)
		var options []fs.OpenOption
		for _, option := range fs.GetConfig(ctx).DownloadHeaders {
			options = append(options, option)
		}
		var in io.ReadCloser
		in, err = operations.Open(ctx, entry, options...)
		if err != nil {
			defer tr.Done(ctx, err)
			return nil,fmt.Errorf("Failed to open file %s: %w",name,err)
		}
		// Account and buffer the transfer
		in = tr.Account(ctx, in).WithBuffer()
		// RCloneFile, tr.Done() is colled in RCloneFile.Close()
		f:=NewRCloneFile(fi,ctx,in,tr)
		//
		if printMsg {
			operations.SyncFprintf(os.Stdout, "a %s\n", name)
		}else{
			fs.Debugf(src,"Adding %s to archive",name)
		}
		//
		return f,nil
	})
}

func GetRemoteFileInfo(ctx context.Context, src fs.Fs,printMsg bool) ArchivesFileInfoList {
	var files ArchivesFileInfoList
	// get all file entries
        walk.Walk(ctx, src, "", false, -1,func(path string, entries fs.DirEntries, err error) error{
                entries.ForObject(func(o fs.Object) {
			fi:=DirEntryToFileInfo(ctx,src,o,printMsg)
			files = append(files, fi)
                })
                return nil
        })
	sort.Stable(files)
	return files
}

func CheckFs(ctx context.Context, remote string) (fs.Fs,string,error){
	var err error
	// check remote
	dst,dstFile := cmd.NewFsFile(remote)
	_, err = dst.NewObject(ctx, dstFile)
	if err == nil {
		// its a file
		return dst,dstFile,nil
	} else if errors.Is(err, fs.ErrorIsDir) {
		// it's a directory
		return dst,"",nil
	} else if ! errors.Is(err, fs.ErrorObjectNotFound) {
		return dst,"",fmt.Errorf("%v",err)
	}
	// remote failed,check parent
	parent,parentFile:=path.Split(remote)
	pDst,_ := cmd.NewFsFile(parent)
	_, err = pDst.NewObject(ctx, "")
	if errors.Is(err, fs.ErrorIsDir) {
		// parent it's a directory
		return pDst,parentFile,nil
	} else {
		return dst,"",fmt.Errorf("%v",err)
	}
}


func ListTest(ctx context.Context, src fs.Fs,srcFile string,dstRemote string) error {
	var dst fs.Fs
	var dstFile string
	var err error
	// check dst
	if dstRemote != "" {
		dst,dstFile,err= CheckFs(ctx,dstRemote)
		// should create an io.WriteCloser for dst here
		if err != nil {
			return fmt.Errorf("Unable to use destination, %v",err)
		}else if dstFile == "" { // a directory
			fmt.Printf("Dir destination Root=%s, File=%s\n",dst.Root(),dstFile)
		}
	}
	//
	fmt.Printf("Format from filename=%s\n",GetFormatFromFile(dstFile))
	//
	items:=GetRemoteFileInfo(ctx,src,dst!=nil)
	for _, item := range items {
		operations.SyncFprintf(os.Stdout, "%s\n",item.NameInArchive)
	}
	return nil
}

// actual function that creates the archive, only to stdout at the moment

func CreateArchive(ctx context.Context, src fs.Fs, srcFile string,dstRemote string) error{
	var dst fs.Fs
	var dstFile string
	var err error
	var files ArchivesFileInfoList
	var compArchive archives.CompressedArchive
	// check id dst is valid
	if dstRemote != "" {
		// writing to a remote, check if valid
		dst,dstFile,err = CheckFs(ctx,dstRemote)
		if err != nil {
			return fmt.Errorf("Unable to use destination, %v",err)
		} else if dstFile == "" {
			return fmt.Errorf("Unable to use destination, filename is missing")
		}
	} else {
		dst=nil
		dstFile=""
	}
	// get archive format
	switch GetFormatFromFile(dstFile){
	case "tar":
		compArchive = archives.CompressedArchive{
			Archival:    archives.Tar{},
		}
	case "tar.gz":
		compArchive = archives.CompressedArchive{
			Compression: archives.Gz{},
			Archival:    archives.Tar{},
		}
	case "tar.bz2":
		compArchive = archives.CompressedArchive{
			Compression: archives.Bz2{},
			Archival:    archives.Tar{},
		}
	case "tar.lz":
		compArchive = archives.CompressedArchive{
			Compression: archives.Lzip{},
			Archival:    archives.Tar{},
		}
	case "tar.lz4":
		compArchive = archives.CompressedArchive{
			Compression: archives.Lz4{},
			Archival:    archives.Tar{},
		}
	case "tar.xz":
		compArchive = archives.CompressedArchive{
			Compression: archives.Xz{},
			Archival:    archives.Tar{},
		}
	case "tar.zst":
		compArchive = archives.CompressedArchive{
			Compression: archives.Zstd{},
			Archival:    archives.Tar{},
		}
	case "tar.br":
		compArchive = archives.CompressedArchive{
			Compression: archives.Brotli{},
			Archival:    archives.Tar{},
		}
	case "tar.sz":
		compArchive = archives.CompressedArchive{
			Compression: archives.Sz{},
			Archival:    archives.Tar{},
		}
	case "zip":
		compArchive = archives.CompressedArchive{
			Archival:    archives.Zip{},
		}
	case "":
		return fmt.Errorf("Format not set and can't be guessed from extension")
	default:
		return fmt.Errorf("Invalid format '%s'",format)
	}
	// get source files
	files=GetRemoteFileInfo(ctx,src,dst!=nil)
	// leave if no files
	if files.Len() == 0 {
		return fmt.Errorf("No files to found in source")
	}
	// create destination
	if dst != nil {
		// create io.Pipe
		pipeReader, pipeWriter := io.Pipe()
		// write to pipewriter in background
		go func() {
			err := compArchive.Archive(ctx, pipeWriter, files)
		   	pipeWriter.CloseWithError(err)
		}()
		// rcat to remote from pipereader
		_,err = operations.Rcat(ctx, dst,dstFile,pipeReader, time.Now(), nil)
		return err
	} else {
		// write to stdout
		return compArchive.Archive(ctx, os.Stdout, files)
	}
}

var commandDefinition = &cobra.Command{
	Use:   "archive source:path dest:path",
	Short: `Archive source file(s) to destination.`,
	// Warning! "|" will be replaced by backticks below
	Long: strings.ReplaceAll(`Creates an archive from the files source:path and saves the archive
to dest:path. If dest:path is missing, it will write to the console.

Valid formats for the --format flag. If format is not set it will
guess it from the extension.

	Format	  Extensions
	------	  -----------
	zip 	  .zip
	tar 	  .tar
	tar.gz 	  .tar.gz, .tgz, .taz
	tar.bz2   .tar.bz2, .tb2, .tbz, .tbz2, .tz2
	tar.lz	  .tar.lz
	tar.xz	  .tar.xz, .txz
	tar.zst	  .tar.zst, .tzst
	tar.br	  .tar.br
	tar.sz	  .tar.sz

The --fullpath flag will set the file name in the archive to the 
full path name. If we have a directory |/sourcedir| with the following:

    file1.txt
    dir1/file2.txt

If we run the command |rclone archive /sourcedir /dest.tar.gz| the 
contents of the archive will be:

    file1.txt
    dir1/file2.txt

If we run the command |rclone archive --fullpath /sourcedir /dest.tar.gz|
the contents of the archive will be:

    sourcedir/file1.txt
    sourcedir/dir1/file2.txt
`, "|", "`"),
	Annotations: map[string]string{
		"versionIntroduced": "v1.68",
		"groups":            "Copy,Filter,Listing",
	},
	Run: func(command *cobra.Command, args []string) {
		var src fs.Fs
		var srcFile,dstRemote string
		if len(args) == 1 { // source only, archive to stdout
			src,srcFile = cmd.NewFsFile(args[0])
			dstRemote=""
		}else if len(args) == 2 {
			src,srcFile = cmd.NewFsFile(args[0])
			dstRemote=args[1]
		}else{
			cmd.CheckArgs(1, 2, command, args)
		}
		//
		cmd.Run(false, false, command, func() error {
			return CreateArchive(context.Background(), src,srcFile,dstRemote)
		})

	},
}

