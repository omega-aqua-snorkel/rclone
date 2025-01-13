package archive

import (
	"context"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"

	"github.com/rclone/rclone/cmd/archive/create"
	"github.com/rclone/rclone/cmd/archive/list"
	"github.com/rclone/rclone/cmd/archive/extract"
)

var (
	t1 = fstest.Time("2017-02-03T04:05:06.499999999Z")
)

// TestMain drives the tests
func TestMain(m *testing.M) {
	fstest.TestMain(m)
}


func TestCheckValidDestination(t *testing.T) {
	var dstFile string
	var err error
	//
	ctx := context.Background()
	r := fstest.NewRun(t)
	// create file
	r.WriteObject(ctx, "file1.txt", "111", t1)
	// test checkValidDestination when file exists 
	_,dstFile,err = create.CheckValidDestination(ctx, r.Fremote, "file1.txt")
	require.NoError(t, err)
	require.Equal(t,"file1.txt",dstFile)
	// test checkValidDestination when file does not exist
	_,dstFile,err = create.CheckValidDestination(ctx, r.Fremote, "file2.txt")
	require.NoError(t, err)
	require.Equal(t,"file2.txt",dstFile,"file2.txt != %s",dstFile)
	// test checkValidDestination when dest is a directory
	_,dstFile,err = create.CheckValidDestination(ctx, r.Fremote, "")
	require.ErrorIs(t, err,fs.ErrorIsDir)
	// test checkValidDestination when dest does not exists
	_,dstFile,err = create.CheckValidDestination(ctx, r.Fremote, "dir/file.txt")
	require.ErrorIs(t, err,fs.ErrorObjectNotFound)
}

func TestArchiveFunctions(t *testing.T) {
	var err error
	//
        ctx := context.Background()
        r := fstest.NewRun(t)
        // create local file system
        f1:=r.WriteFile("file1.txt", "content 1", t1)
        f2:=r.WriteFile("dir1/sub1.txt", "sub content 1", t1)
        f3:=r.WriteFile("dir2/sub2.txt", "sub content 2", t1)
        // create archive
        err = create.ArchiveCreate(ctx,r.Flocal,"",r.Flocal,"test.zip","",false)
        require.NoError(t, err)
	// list archive
	err = list.ArchiveList(ctx,r.Flocal,"test.zip",false)
        require.NoError(t, err)
	// extract archive
	err = extract.ArchiveExtract(ctx,r.Flocal,"test.zip",r.Fremote,"")
        require.NoError(t, err)
	// check files
	fstest.CheckListingWithPrecision(t, r.Fremote, []fstest.Item{f1, f2, f3}, nil, fs.ModTimeNotSupported)
}
