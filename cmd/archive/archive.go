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
	format = string("zip")
)

// RCloneMetadata

func init() {
	cmd.Root.AddCommand(commandDefinition)
	cmdFlags := commandDefinition.Flags()
	flags.StringVarP(cmdFlags, &format, "format", "",format, "Compress the archive using the selected format. Valid formats: zip,tar,tar.gz,tar.bz2,tar.lz,tar.lz4,tar.xz,tar.zst,tar.br,tar.sz","")
}


func DirEntryToFileInfo(ctx context.Context,entry fs.Object) archives.FileInfo {
	// get entry type
	dirType := reflect.TypeOf((*fs.Directory)(nil)).Elem()
	// fill structure
        name := entry.Remote()
        size := entry.Size()
        mtime := entry.ModTime(ctx)
        isDir := reflect.TypeOf(entry).Implements(dirType)
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
		return f,nil
	})
}

// Test function to get valid path in remote, returns valid path, missing paths

func GetValidPath(ctx context.Context,remote string)(string,string,error){
	var root,missing string
	var err error
	//
	root=remote
	missing=""
	//
	fmt.Printf( "Check %s\n",remote)
	src,_ := cmd.NewFsFile(root)
	_, err = src.List(ctx, "/")
	// valid source
	if err == nil {
		fmt.Printf( "  Root=%s Missing=%s Error=%w\n",remote,"",err)
		return remote,"",nil
	}
	// invalid remote, find valid one
	for err != nil {
		fmt.Printf( "  Root=%s Missing=%s Error=%w\n",root,missing,err)
		missing=path.Join(path.Base(root),missing)
		root=path.Dir(root)
		//
		src,_ := cmd.NewFsFile(root)
		_, err = src.List(ctx, "/")
	}
	//
	fmt.Printf( "  Root=%s Missing=%s Error=%w\n",root,missing,err)
	return root,missing,err
}

// Test function walks fs.Fs and converts objects to archives.FileInfo array

func GetRemoteFileInfo(ctx context.Context, src fs.Fs) ArchivesFileInfoList {
	var files ArchivesFileInfoList
	// get all file entries
        walk.Walk(ctx, src, "", false, -1,func(path string, entries fs.DirEntries, err error) error{
                entries.ForObject(func(o fs.Object) {
			fi:=DirEntryToFileInfo(ctx,o)
			files = append(files, fi)
                })
                return nil
        })
	sort.Stable(files)
	return files
}

// test function, list files in src as array of archives.FileInfo

func check_fs(ctx context.Context, remote string) (fs.Fs,string,error){
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
	parent:=path.Dir(remote)
	parentFile:=path.Base(remote)
	pDst,_ := cmd.NewFsFile(parent)
	_, err = pDst.NewObject(ctx, "")
	if errors.Is(err, fs.ErrorIsDir) {
		// parent it's a directory
		return pDst,parentFile,nil
	} else {
		return dst,"",fmt.Errorf("%v",err)
	}
}


func list_test(ctx context.Context, src fs.Fs,dstRemote string) error {
	// check dst
	if dstRemote != "" {
		dst,dstFile,err:= check_fs(ctx,dstRemote)
		// should create an io.WriteCloser for dst here
		if err != nil {
			return fmt.Errorf("Unable to use destination, %v",err)
		}else{
			fmt.Printf("Destination not implemented: Root:=%s, File=%s\n",dst.Root(),dstFile)
		}
	}
	//

	items:=GetRemoteFileInfo(ctx,src)
	for _, item := range items {
		operations.SyncFprintf(os.Stdout, "%s\n",item.NameInArchive)
	}
	return nil
}

// actual function that creates the archive, only to stdout at the moment

func create_archive(ctx context.Context, src fs.Fs, dstRemote string) error{
	var files ArchivesFileInfoList
	var compArchive archives.CompressedArchive
	// get archive format
	switch strings.ToLower(format){
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
	default:
		return fmt.Errorf("Invalid format: %s",format)
	}
	// get source files
        walk.Walk(ctx, src, "", false, -1,func(path string, entries fs.DirEntries, err error) error{
                entries.ForObject(func(o fs.Object) {
			fi:=DirEntryToFileInfo(ctx,o)
			files = append(files, fi)
                })
                return nil
        })
	// sort fource files
	sort.Stable(files)
	// leave if no files
	if files.Len() == 0 {
		return fmt.Errorf("No files to found in source")
	}
	// create destination
	if dstRemote != "" {
		dst,dstFile,err:= check_fs(ctx,dstRemote)
		if err != nil {
			return fmt.Errorf("Unable to use destination, %v",err)
		}
		// create io.Pipe
		pipeReader, pipeWriter := io.Pipe()
		go func() {
			err := compArchive.Archive(ctx, pipeWriter, files)
		   	pipeWriter.CloseWithError(err)
		}()
		_,err = operations.Rcat(ctx, dst,dstFile,pipeReader, time.Now(), nil)
		return err
	} else {
		return compArchive.Archive(ctx, os.Stdout, files)
	}
}




var commandDefinition = &cobra.Command{
	Use:   "archive source:path dest:path",
	Short: `Archive source file(s) to destination.`,
	// Warning! "|" will be replaced by backticks below
	Long: strings.ReplaceAll(``, "|", "`"),
	Annotations: map[string]string{
		"versionIntroduced": "v1.68",
		"groups":            "Copy,Filter,Listing",
	},
	Run: func(command *cobra.Command, args []string) {
		var src fs.Fs
		var dstRemote string
		if len(args) == 1 { // source only, archive to stdout
			src = cmd.NewFsSrc(args)
			dstRemote=""
		}else if len(args) == 2 {
			src = cmd.NewFsSrc(args)
			dstRemote=args[1]
		}else{
			cmd.CheckArgs(1, 2, command, args)
		}
		//
		cmd.Run(false, false, command, func() error {
			return create_archive(context.Background(), src,dstRemote)
		})

	},
}

