package archive

import (
	"context"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"

	"github.com/rclone/rclone/cmd/archive/create"
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

func TestCreateArchive(t *testing.T) {
        ctx := context.Background()
        r := fstest.NewRun(t)
        // create local file
        r.WriteFile("root.txt", "root", t1)
        r.WriteFile("dir1/sub1.txt", "111", t1)
        r.WriteFile("dir2/sub2.txt", "222", t1)
        // try and create archive
	err := create.ArchiveCreate(ctx,r.Flocal,"",r.Fremote,"text.tgz","",true)
        require.NoError(t, err)
}
