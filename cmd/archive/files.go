
package archive

import (

	"fmt"
	"path"
	"io"
	"io/fs"
	"strings"
	"strconv"
	"time"
	"context"
        "github.com/rclone/rclone/fs/accounting"
	"github.com/mholt/archives"
	"archive/tar"

)

// not using this, not all backends have uid/gid so is this even worth it?

func MetadataToHeader(metadata map[string]string,header *tar.Header){
	var val string
	var ok bool
	var err error
	var mode,uid,gid int64
	var atime,ctime time.Time
	// check if metadata is valid
	mode=0644
	uid=0
	gid=0
	atime=time.Unix(0,0)
	ctime=time.Unix(0,0)
	if metadata != nil {
		// mode
	        val, ok = metadata["mode"]
	        if ok == false {
			mode=0644
		}else{
		        mode, err = strconv.ParseInt(val, 8, 64)
		        if err != nil { mode = 0664 }
		}
		// uid
	        val, ok = metadata["uid"]
	        if ok == false {
			uid=0
		}else{
		        uid, err = strconv.ParseInt(val, 10, 32)
		        if err != nil { uid = 0 }
		}
		// gid
	        val, ok = metadata["gid"]
	        if ok == false {
			gid=0
		}else{
		        gid, err = strconv.ParseInt(val, 10, 32)
		        if err != nil { gid = 0 }
		}
		// access time
	        val, ok := metadata["atime"]
	        if ok == false {
			atime=time.Unix(0,0)
		}else{
		        atime, err = time.Parse(time.RFC3339Nano,val)
		        if err != nil { atime = time.Unix(0,0) }
		}
	}
	//
	header.Mode = mode
	header.Uid = int(uid)
	header.Gid = int(gid)
	header.AccessTime = atime
	header.ChangeTime = ctime
}


// FileInfoFS
// fs.FileInfo interface, required for mholt/archives

type FileInfoFS interface {
	fs.FileInfo
}

type FileInfoFSImpl struct{
        name string
        size int64
        mtime time.Time
        isDir bool
	header *tar.Header // hate this, just for uid/gid/gname/uname
}

func (a *FileInfoFSImpl) Name() string {
	return a.name
}

func (a *FileInfoFSImpl) Size() int64 {
	return a.size
}

func (a *FileInfoFSImpl) Mode() fs.FileMode {
	return fs.FileMode(a.header.Mode)
}

func (a *FileInfoFSImpl) ModTime() time.Time {
	return a.mtime
}

func (a *FileInfoFSImpl) IsDir() bool {
	return a.isDir
}

func (a *FileInfoFSImpl) Sys() any {
	return a.header
}

func (a *FileInfoFSImpl) String() string {
        return fmt.Sprintf("Name=%v Size=%v IsDir=%v UID=%v GID=%v", a.Name(),a.Size(),a.IsDir(),a.header.Uid,a.header.Gid)
}

// RCloneFile
// fs.File interface, required for mholt/archives

type RCloneFile interface {
	fs.File
}

type RCloneFileImpl struct{
	entry FileInfoFS
	ctx context.Context
	reader io.ReadCloser
	transfer *accounting.Transfer
	err error
}

func NewRCloneFile(entry FileInfoFS,ctx context.Context,reader io.ReadCloser,transfer *accounting.Transfer) RCloneFile {
        var f = new(RCloneFileImpl)
	//
	f.entry=entry
	f.ctx=ctx
	f.reader=reader
	f.transfer=transfer
	f.err=nil
	//
        return f
}

func (a *RCloneFileImpl) Stat() (fs.FileInfo, error){
	return fs.FileInfo(a.entry),nil
}

func (a *RCloneFileImpl) Read(data []byte) (int, error){
	if a.reader == nil {
		a.err=fmt.Errorf("File %s not open",a.entry.Name())
		return 0, a.err
	} else {
		i,err:=a.reader.Read(data)
		a.err=err
		return i,a.err
	}
}
func (a *RCloneFileImpl) Close() error {
	// close file
	if a.reader == nil {
		a.err=fmt.Errorf("File %s not open",a.entry.Name())
	} else {
		a.err=a.reader.Close()
	}
	// close transfer
	if a.transfer != nil {
		a.transfer.Done(a.ctx,a.err)
	}
	return a.err
}

// RCloneOpener
// Open the rclone file

type RCloneOpener func(fi FileInfoFS) (RCloneFile,error)

// NewArchivesFileInfo
// Creates a archives.FileInfo from the given information

func NewArchivesFileInfo(name string,size int64,mtime time.Time,isDir bool,opener RCloneOpener) archives.FileInfo {
	var fi=new (FileInfoFSImpl)
	//
	fi.name = name
	fi.size = size
	fi.mtime = mtime
	fi.isDir = isDir
	fi.header = new(tar.Header)
	fi.header.Mode = 0666
	fi.header.Uid = 0
	fi.header.Gid = 0
	fi.header.Uname = "root"
	fi.header.Gname = "root"
	fi.header.AccessTime = mtime
	fi.header.ChangeTime = mtime
	//
	return archives.FileInfo{
		FileInfo: fi,
		NameInArchive: name,
		LinkTarget: "",
		Open: func() (fs.File, error) {
			f,err := opener(fi)
			if err != nil {
				return nil, err
			} else {
				return f,nil
			}
		},
	}
}

// ArchivesFileInfoList
// Array of archives.FileInfo

type ArchivesFileInfoList []archives.FileInfo

func (a ArchivesFileInfoList) Len() int {
	return len(a)
}

func (a ArchivesFileInfoList) Less(i, j int) bool {
	var dir1= path.Dir(a[i].NameInArchive)
	var dir2= path.Dir(a[j].NameInArchive)

	if dir1 < dir2 {
		return true
	} else if dir1 > dir2 {
		return false
	}else if a[i].FileInfo.IsDir() == a[j].FileInfo.IsDir() {
		return strings.Compare(a[i].NameInArchive,a[j].NameInArchive) < 0
	} else {
		return a[j].FileInfo.IsDir()
	}
}

func (a ArchivesFileInfoList) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}
