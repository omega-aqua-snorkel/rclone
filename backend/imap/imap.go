// Package imap implements a provider for imap servers.
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/lib/env"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

var (
	currentUser          = env.CurrentUser()
	imapHost             = "imap.rclone.org"
	securityNone         = 0
	securityStartTLS     = 1
	securityTLS          = 2
	messageNameRegEx     = regexp.MustCompile(`(\d+)\.R([^\.]+)\.([^,]+),S\=(\d+)-2,(.*)`)
	remoteRegex          = regexp.MustCompile(`(?:(?P<parent>.*)\/)?(?P<file>\d+\.R[^\.]+\.[^,]+,S\=\d+-2,.*)|(?P<dir>.*)`)
	errorInvalidFileName = errors.New("invalid file name")
	errorInvalidMessage  = errors.New("invalid IMAP message")
	errorReadingMessage  = errors.New("failed to read message")
)

// Options defines the configuration for this backend
type Options struct {
	Host               string `config:"host"`
	User               string `config:"user"`
	Pass               string `config:"pass"`
	Port               string `config:"port"`
	AskPassword        bool   `config:"ask_password"`
	Security           string `config:"security"`
	InsecureSkipVerify bool   `config:"no_check_certificate"`
}

// Register with Fs
func init() {
	fsi := &fs.RegInfo{
		Name:        "imap",
		Description: "IMAP",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "host",
			Help:      "IMAP host to connect to.\n\nE.g. \"imap.example.com\".",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "user",
			Help:      "IMAP username.",
			Default:   currentUser,
			Sensitive: true,
		}, {
			Name:    "port",
			Help:    "IMAP port number.",
			Default: 143,
		}, {
			Name:       "pass",
			Help:       "IMAP password.",
			IsPassword: true,
		}, {
			Name:    "security",
			Help:    "IMAP Connection type: none,starttls,tls",
			Default: "starttls",
		}, {
			Name:    "no_check_certificate",
			Help:    "Skip server certificate verification",
			Default: true,
		}},
	}
	fs.Register(fsi)
}

// Utility functions

func createHash(reader io.ReadCloser, t ...hash.Type) (map[hash.Type]string, error) {
	if len(t) == 0 {
		t = []hash.Type{hash.MD5}
	}
	// create hasher
	set := hash.NewHashSet(t...)
	hasher, err := hash.NewMultiHasherTypes(set)
	if err != nil {
		return nil, err
	}
	// do hash
	if _, err := io.Copy(hasher, reader); err != nil {
		return nil, err
	}
	// close reader
	err = reader.Close()
	if err != nil {
		return nil, err
	}
	// save hashes to map
	hashes := map[hash.Type]string{}
	for _, curr := range t {
		// get the hash
		checksum, err := hasher.SumString(hash.MD5, false)
		if err == nil {
			hashes[curr] = checksum
		}
	}
	//
	return hashes, nil
}

func isMessage(reader io.ReadCloser) error {
	_, err := mail.ReadMessage(reader)
	if err != nil {
		return errorInvalidMessage
	}
	// close reader
	err = reader.Close()
	if err != nil {
		return err
	}
	return nil
}

func reverse(input string) string {
	// Get Unicode code points.
	n := 0
	rune := make([]rune, len(input))
	for _, r := range input {
		rune[n] = r
		n++
	}
	rune = rune[0:n]
	// Reverse
	for i := 0; i < n/2; i++ {
		rune[i], rune[n-1-i] = rune[n-1-i], rune[i]
	}
	// Convert back to UTF-8.
	return string(rune)
}

func topDir(input string) (string, string) {
	right, top := path.Split(reverse(input))
	return strings.Trim(reverse(top), "/"), strings.Trim(reverse(right), "/")
}

func removeRoot(root, element string) string {
	// remove leading/trailing slashes from element and root
	root = strings.Trim(root, "/")
	element = strings.Trim(element, "/")
	// check if element matches
	if root == "" {
		return element
	} else if root == element {
		return ""
	} else if strings.HasPrefix(element, root+"/") {
		return strings.TrimPrefix(element, root+"/")
	}
	return element
}

func getMatches(root string, list []string) []string {
	var value string
	// remove leading/trailing slashes from root
	root = strings.Trim(root, "/")
	// check list for matching prefix
	result := []string{}
	for _, element := range list {
		// remove leading/trailing slashes from element
		element = strings.Trim(element, "/")
		// check if element matches
		if root == "" {
			// looking in root, return top dir
			value, _ = topDir(element)
		} else if strings.HasPrefix(element, root+"/") {
			// check if element starts with root
			value, _ = topDir(strings.TrimPrefix(element, root+"/"))
		} else {
			// no match skip
			value = ""
		}
		// add if value not empty and not in result array
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	//
	return result
}

func searchCriteriaFromDescriptor(info *descriptor) *imap.SearchCriteria {
	// init criteria for search
	criteria := imap.NewSearchCriteria()
	// include messages within date range (1 day before and after, imap uses date only no time)
	criteria.Since = info.ModTime().AddDate(0, -1, 0)
	criteria.Before = info.ModTime().AddDate(0, 1, 0)
	// include messages with size between size-50 and size+50
	criteria.Larger = uint32(info.Size() - 50)
	criteria.Smaller = uint32(info.Size() + 50)
	//
	return criteria
}

func searchCriteriaFromName(parent, name string) (*imap.SearchCriteria, error) {
	// extract info from file name
	info, err := nameToDescriptor(parent, name)
	// leave if not valid file name
	if err != nil {
		return nil, err
	}
	return searchCriteriaFromDescriptor(info), nil
}

func regexToMap(r *regexp.Regexp, str string) map[string]string {
	results := map[string]string{}
	matches := r.FindStringSubmatch(str)
	if matches != nil {
		for i, name := range r.SubexpNames() {
			if i != 0 {
				results[name] = matches[i]
			}
		}
	}
	return results
}

func fetchEntries(f *Fs, dir string) (entries fs.DirEntries, err error) {
	var parent string
	var file string
	var mailboxes []string
	var criteria *imap.SearchCriteria
	// add root to dir
	items := []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope, imap.FetchItem("BODY.PEEK[]")}
	seqset := new(imap.SeqSet)
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return nil, err
	}
	// connected, logout on exit
	defer client.Logout()
	// parse
	groups := regexToMap(remoteRegex, path.Join(f.root, dir))
	file = groups["file"]
	if file == "" {
		parent = groups["dir"]
	} else {
		parent = groups["parent"]
	}
	// check parent and file
	if parent == "" {
		// find message in root, impossible
		if file != "" {
			fs.Debugf(nil, "List %s: matching %s (there are no messages in root)", f.root, file)
			return entries, nil
		}
		// list mailboxes in root
		fs.Debugf(nil, "List root")
	} else if !client.HasMailbox(parent) {
		// mailbox does not exist, leave
		fs.Debugf(nil, "List %s:%s, not found", f.name, parent)
		return nil, fs.ErrorDirNotFound
	} else if file == "" {
		// list mailbox contents
		fs.Debugf(nil, "List %s:%s", f.name, parent)
	} else {
		fs.Debugf(nil, "List %s:%s matching %s", f.name, parent, file)
		// list a message in mailbox,file must be in rclone maildir format
		criteria, err = searchCriteriaFromName(parent, file)
		if err == errorInvalidFileName {
			// searching for filename not in maildir format, will never be found
			return entries, nil
		} else if err != nil {
			// unknown error
			return nil, err
		}
	}
	// get mailboxes
	mailboxes, err = client.ListMailboxes(parent)
	for _, name := range mailboxes {
		if file == "" {
			d := fs.NewDir(path.Join(dir, name), time.Unix(0, 0))
			entries = append(entries, d)
		}
	}
	// root has no messages
	if parent == "" && file == "" {
		return entries, nil
	}
	// get message count in mailbox
	mboxStatus, err := client.GetMailboxStatus(parent)
	if err != nil {
		// error getting mailbox information
		return nil, err
	} else if mboxStatus.Messages == 0 {
		// mailbox is empty
		return entries, nil
	} else if criteria != nil {
		// searching for a message
		ids, err := client.Search(parent, criteria)
		// error searching
		if err != nil {
			return nil, err
		}
		// no matches
		if len(ids) == 0 {
			return entries, nil
		}
		// set matches
		seqset.AddNum(ids...)
		fs.Debugf(nil, "Fetch from %s with %d messages - IDS=%s", strings.Trim(parent, "/"), mboxStatus.Messages, strings.Join(strings.Fields(fmt.Sprint(ids)), ", "))
	} else {
		// get all messages in mailbox
		seqset.AddRange(1, mboxStatus.Messages)
		fs.Debugf(nil, "Fetch all from %s with %d messages", strings.Trim(parent, "/"), mboxStatus.Messages)
	}
	// get messages matching ids
	err = client.Fetch(parent, seqset, items, func(msg *imap.Message) {
		info, err := messageToDescriptor(parent, "", msg)
		if err != nil {
			fs.Debugf(nil, "Error converting message to object: %s", err.Error())
		} else if file == "" || info.Matches(file) {
			obj := &Object{fs: f, seqNum: msg.SeqNum, info: info, hashes: map[hash.Type]string{hash.MD5: info.Checksum()}}
			fs.Debugf(nil, "Adding object root=%s name=%s", f.root, obj.Remote())
			entries = append(entries, obj)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch messages: %w", err)
	}
	return entries, nil
}

// ------------------------------------------------------------
// Descriptor
// ------------------------------------------------------------

type descriptor struct {
	parent string
	name   string
	date   time.Time
	md5sum string
	size   int64
	flags  []string
}

func newDescriptor(parent string, name string, date time.Time, checksum string, size int64, flags []string) (*descriptor, error) {
	if name == "" {
		name = fmt.Sprintf("%d.R%s.%s,S=%d-2,", date.UTC().Unix(), checksum, imapHost, size)
		if slices.Contains(flags, imap.SeenFlag) {
			name += "S"
		}
		if slices.Contains(flags, imap.AnsweredFlag) {
			name += "A"
		}
		if slices.Contains(flags, imap.DeletedFlag) {
			name += "T"
		}
		if slices.Contains(flags, imap.DraftFlag) {
			name += "D"
		}
		if slices.Contains(flags, imap.FlaggedFlag) {
			name += "F"
		}
	}
	return &descriptor{
		parent: parent,
		name:   name,
		date:   date.UTC(),
		md5sum: checksum,
		size:   size,
		flags:  flags,
	}, nil
}

func readerToDescriptor(parent string, name string, date time.Time, reader io.Reader, size int64, flags []string) (*descriptor, error) {
	seeker, isSeeker := reader.(io.ReadSeeker)
	if !isSeeker {
		fs.Debugf(nil, "readerToMessage - not a seeker!!")
		return nil, errorReadingMessage
	}
	// check if message
	err := isMessage(io.NopCloser(seeker))
	if err != nil {
		return nil, errorInvalidMessage
	}
	// rewind to calculate checksum
	_, err = seeker.Seek(0, io.SeekStart)
	if err != nil {
		return nil, errorReadingMessage
	}
	// calc md5 checksum
	hashes, err := createHash(io.NopCloser(reader), hash.MD5)
	if err != nil {
		return nil, errorReadingMessage
	}
	// get the hash
	checksum, found := hashes[hash.MD5]
	if !found {
		return nil, errorReadingMessage
	}
	// rewind
	_, err = seeker.Seek(0, io.SeekStart)
	if err != nil {
		return nil, errorReadingMessage
	}
	//
	return newDescriptor(parent, name, date, checksum, size, flags)
}

func messageToDescriptor(parent string, name string, msg *imap.Message) (*descriptor, error) {
	var reader io.Reader
	// get body io.Reader (should be first value in msg.Body)
	for _, curr := range msg.Body {
		reader = curr
		break
	}
	if reader == nil {
		return nil, errorReadingMessage
	}
	// calc md5 checksum
	hashes, err := createHash(io.NopCloser(reader), hash.MD5)
	if err != nil {
		return nil, errorReadingMessage
	}
	// get the hash
	checksum, found := hashes[hash.MD5]
	if !found {
		return nil, errorReadingMessage
	}
	//
	return newDescriptor(parent, name, msg.InternalDate, checksum, int64(msg.Size), msg.Flags)
}

func objectToDescriptor(ctx context.Context, parent string, o fs.Object) (info *descriptor, err error) {
	// try and check if name is valid maildir name
	info, err = nameToDescriptor(parent, o.Remote())
	if err == nil {
		return info, nil
	}
	// not a valid name, check if a valid message
	reader, err := o.Open(ctx)
	if err != nil {
		return nil, errorReadingMessage
	}
	// check if message
	err = isMessage(reader)
	if err != nil {
		return nil, errorInvalidMessage
	}
	// valid message, get checksum
	reader, err = o.Open(ctx)
	if err != nil {
		return nil, errorReadingMessage
	}
	hashes, err := createHash(reader, hash.MD5)
	if err != nil {
		return nil, errorReadingMessage
	}
	checksum, found := hashes[hash.MD5]
	if !found {
		return nil, errorReadingMessage
	}
	return newDescriptor(parent, o.Remote(), o.ModTime(ctx).UTC(), checksum, o.Size(), []string{})
}

func nameToDescriptor(parent, name string) (*descriptor, error) {
	matches := messageNameRegEx.FindStringSubmatch(path.Base(name))
	if matches == nil {
		return nil, errorInvalidFileName
	}
	// get date
	i, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return nil, errorInvalidFileName
	}
	date := time.Unix(i, 0).UTC()
	// get hash
	checksum := matches[2]
	// get size
	size, err := strconv.ParseInt(matches[4], 10, 32)
	if err != nil {
		return nil, errorInvalidFileName
	}
	// get flags
	flags := []string{}
	if strings.Contains(matches[5], "S") {
		flags = append(flags, imap.SeenFlag)
	}
	if strings.Contains(matches[5], "A") {
		flags = append(flags, imap.AnsweredFlag)
	}
	if strings.Contains(matches[5], "T") {
		flags = append(flags, imap.DeletedFlag)
	}
	if strings.Contains(matches[5], "D") {
		flags = append(flags, imap.DraftFlag)
	}
	if strings.Contains(matches[5], "F") {
		flags = append(flags, imap.FlaggedFlag)
	}
	//
	return newDescriptor(parent, name, date, checksum, size, flags)
}

func (i *descriptor) Equal(o *descriptor) bool {
	return i.date == o.date && i.md5sum == o.md5sum && i.size == o.size
}

func (i *descriptor) Name() string {
	return i.name
}

func (i *descriptor) MaildirName(flags bool) string {
	name := fmt.Sprintf("%d.R%s.%s,S=%d-2,", i.date.UTC().Unix(), i.md5sum, imapHost, i.size)
	if !flags {
		return name
	}
	// include flags
	if slices.Contains(i.flags, imap.SeenFlag) {
		name += "S"
	}
	if slices.Contains(i.flags, imap.AnsweredFlag) {
		name += "A"
	}
	if slices.Contains(i.flags, imap.DeletedFlag) {
		name += "T"
	}
	if slices.Contains(i.flags, imap.DraftFlag) {
		name += "D"
	}
	if slices.Contains(i.flags, imap.FlaggedFlag) {
		name += "F"
	}
	//
	return name

}

func (i *descriptor) Matches(name string) bool {
	matches := messageNameRegEx.FindStringSubmatch(path.Base(name))
	if matches == nil {
		return false
	}
	// get date
	idate, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return false
	}
	date := time.Unix(idate, 0).UTC()
	// get hash
	checksum := matches[2]
	// get size
	size, err := strconv.ParseInt(matches[4], 10, 64)
	if err != nil {
		return false
	}
	return i.date == date && i.md5sum == checksum && i.size == size
}

func (i *descriptor) ModTime() time.Time {
	return i.date
}

func (i *descriptor) Checksum() string {
	return i.md5sum
}

func (i *descriptor) Host() string {
	return imapHost
}

func (i *descriptor) Size() int64 {
	return i.size
}

func (i *descriptor) IsFlagSet(value string) bool {
	return slices.Contains(i.flags, value)
}

// ------------------------------------------------------------
// Fs
// ------------------------------------------------------------

// Fs represents a remote IMAP server
type Fs struct {
	name     string       // name of this remote
	root     string       // the path we are working on if any
	opt      Options      // parsed config options
	features *fs.Features // optional features
	//
	host       string
	port       int
	user       string
	pass       string
	security   int
	skipVerify bool
}

// Name of this fs
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	if f.security == securityTLS {
		return fmt.Sprintf("imaps://%s:%d", f.host, f.port)
	}
	return fmt.Sprintf("imap://%s:%d", f.host, f.port)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// List returns the entries in the directory
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	return fetchEntries(f, dir)
}

// Precision of the object storage system
func (f *Fs) Precision() time.Duration {
	return time.Second
}

func (f *Fs) findObject(ctx context.Context, info *descriptor) (*Object, error) {
	var o *Object
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return nil, fmt.Errorf("failed append: %w", err)
	}
	// get search criteria
	criteria := searchCriteriaFromDescriptor(info)
	items := []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope, imap.FetchItem("BODY.PEEK[]")}
	// connected, logout on exit
	defer client.Logout()
	// search messages
	ids, err := client.Search(info.parent, criteria)
	if err != nil {
		return nil, fs.ErrorObjectNotFound
	} else if len(ids) == 0 {
		return nil, fs.ErrorObjectNotFound
	}
	// messages found, check matching one
	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)
	err = client.Fetch(info.parent, seqset, items, func(msg *imap.Message) {
		// leave if we found it
		if o != nil {
			return
		}
		//
		currInfo, err := messageToDescriptor(info.parent, "", msg)
		if err != nil {
			return
		} else if info.Equal(currInfo) {
			o = &Object{fs: f, seqNum: msg.SeqNum, info: currInfo, hashes: map[hash.Type]string{hash.MD5: currInfo.Checksum()}}
		}
	})
	//
	if err != nil {
		return nil, err
	} else if o == nil {
		return nil, fs.ErrorObjectNotFound
	}
	//
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	var info *descriptor
	var err error
	//
	fs.Debugf(nil, "NewObject remote=%s, root=%s", remote, f.root)
	srcObj, hasSource := ctx.Value(operations.SourceObjectKey).(fs.Object)
	if hasSource {
		// srcObj is valid, for message using date,checksum,size instead of name
		info, err = objectToDescriptor(ctx, f.root, srcObj)
	} else {
		// try and get descriptor from name
		info, err = nameToDescriptor(f.root, remote)

	}
	// leave if no descriptor
	if err != nil {
		return nil, fserrors.NoRetryError(err)
	}
	return f.findObject(ctx, info)
}

// Put the object
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	var info *descriptor
	var err error
	// check if srcObject available
	fs.Debugf(nil, "put %s,root=%s ", src.Remote(), f.root)
	srcObj, hasSource := ctx.Value(operations.SourceObjectKey).(fs.Object)
	if hasSource {
		fs.Debugf(nil, "found SourceObjectKey in context, convert to descriptor")
		info, err = objectToDescriptor(ctx, f.root, srcObj)
	} else {
		err = errorReadingMessage
	}
	// check if name is valid maildir name
	if err != nil {
		fs.Debugf(nil, "failed getting descriptor from context,use name")
		info, err = nameToDescriptor(f.root, src.Remote())
	}
	// check if we can create info from reader
	if err != nil {
		fs.Debugf(nil, "failed getting descriptor from context and name,use reader")
		info, err = readerToDescriptor(f.root, src.Remote(), src.ModTime(ctx), in, src.Size(), []string{})
	}
	// leave if unable to create info
	if err != nil {
		return nil, fserrors.NoRetryError(err)
	}
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return nil, fmt.Errorf("failed append: %w", err)
	}
	// connected, logout on exit
	defer client.Logout()
	// mkdir if not found
	mailbox := path.Join(info.parent, path.Dir(src.Remote()))
	if !client.HasMailbox(mailbox) {
		fs.Debugf(nil, "Create mailbox %s", mailbox)
		err = client.CreateMailbox(mailbox)
		if err != nil {
			return nil, fserrors.NoRetryError(fmt.Errorf("failed append: %w", err))
		}
	}
	// upload message
	err = client.Save(mailbox, info.ModTime(), info.Size(), in, info.flags)
	if err != nil {
		return nil, fmt.Errorf("failed append: %w", err)
	}
	return f.findObject(ctx, info)
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

// Mkdir creates the directory if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) (err error) {
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return err
	}
	// connected, logout on exit
	defer client.Logout()
	// check if mailbox exists
	root := path.Join(f.root, dir)
	if client.HasMailbox(root) {
		return nil
	}
	// create the mailbox
	return client.CreateMailbox(root)
}

// Rmdir deletes a directory
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) (err error) {
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return err
	}
	// connected, logout on exit
	defer client.Logout()
	// check if mailbox exists
	root := path.Join(f.root, dir)
	if !client.HasMailbox(root) {
		return nil
	}
	// create the mailbox
	err = client.DeleteMailbox(root)
	if err != nil {
		return fserrors.NoRetryError(err)
	}
	return nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server-side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	srcPath := path.Join(srcFs.root, srcRemote)
	dstPath := path.Join(f.root, dstRemote)
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return err
	}
	// connected, logout on exit
	defer client.Logout()
	// check if mailbox exists
	if client.HasMailbox(dstPath) {
		return fs.ErrorDirExists
	}
	// do rename
	return client.RenameMailbox(srcPath, dstPath)
}

// Hashes - valid MD5,SHA1
func (f *Fs) Hashes() hash.Set {
	hashSet := hash.NewHashSet()
	hashSet.Add(hash.SHA1)
	hashSet.Add(hash.MD5)
	return hashSet
}

// ------------------------------------------------------------
// Object
// ------------------------------------------------------------

// Object describes an IMAP message
type Object struct {
	fs     *Fs
	seqNum uint32
	info   *descriptor
	hashes map[hash.Type]string
}

// Equal compare objects
func (o *Object) Equal(v *Object) bool {
	return o.info.Equal(v.info)
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String version of o
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.Remote()
}

// Remote returns the remote path
func (o *Object) Remote() string {
	var name = path.Join(o.info.parent, o.info.Name())
	//
	if name == o.fs.root {
		return path.Base(o.info.name)
	}
	return removeRoot(o.fs.root, name)
}

// Hash returns the hash of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	// find hash in map
	checksum, found := o.hashes[t]
	// hash found in cache
	if found {
		return checksum, nil
	}
	// get reader
	reader, err := o.Open(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to calculate %v: %w", t, err)
	}
	// create hash
	hashes, err := createHash(reader, t)
	if err != nil {
		return "", hash.ErrUnsupported
	}
	// get the hash
	checksum, found = hashes[t]
	if !found {
		return "", hash.ErrUnsupported
	}
	o.hashes[t] = checksum
	// return hash
	return checksum, err
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.info.Size()
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.info.ModTime()
}

// SetModTime sets the modification time of the object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	fs.Debugf(o.fs, "SetModTime is not supported")
	return nil
}

// Storable returns a boolean as to whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	var msg *imap.Message
	//
	fs.Debugf(nil, "Open remote=%s, root=%s", o.Remote(), o.fs.root)
	// connect to imap server
	client, err := newMailClient(o.fs)
	if err != nil {
		return nil, err
	}
	// connected, logout on exit
	defer client.Logout()
	// fetch message
	fs.Debugf(nil, "before open %s - name=%s", o.info.parent, o.Remote())
	msg, err = client.FetchSingle(path.Join(o.info.parent, path.Dir(o.info.name)), o.seqNum, []imap.FetchItem{imap.FetchItem("BODY.PEEK[]")})
	if err != nil {
		return nil, err
	}
	// there should be just one body
	for _, literal := range msg.Body {
		return io.NopCloser(literal), nil

	}
	return nil, fmt.Errorf("failed to get io.Reader: body not found")
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	// connect to imap server
	client, err := newMailClient(o.fs)
	if err != nil {
		return err
	}
	// connected, logout on exit
	defer client.Logout()
	// delete the message
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(o.seqNum)
	// set flags to deleted
	err = client.SetFlags(path.Join(o.fs.root, o.info.parent), seqSet, imap.DeletedFlag)
	if err != nil {
		return err
	}
	// expunge mailbox
	return client.ExpungeMailbox(path.Join(o.fs.root, o.info.parent))
}

// MimeType returns the mime type of the file
// In this case all messages are message/rfc822
func (o *Object) MimeType(ctx context.Context) string {
	return "message/rfc822"
}

// Update an object
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	return fmt.Errorf("not supported")
}

// Matches compares object to a messageInfo
func (o *Object) Matches(info *descriptor) bool {
	return info.MaildirName(false) == o.info.MaildirName(false)
}

// NewFs constructs an Fs from the path.
//
// The returned Fs is the actual Fs, referenced by remote in the config
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	var security int
	var port int
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	// user
	user := opt.User
	if user == "" {
		user = currentUser
	}
	// password
	pass := ""
	if opt.AskPassword && opt.Pass == "" {
		pass = config.GetPassword("IMAP server password")
	} else {
		pass, err = obscure.Reveal(opt.Pass)
		if err != nil {
			return nil, fmt.Errorf("NewFS decrypt password: %w", err)
		}
	}
	// security
	opt.Security = strings.TrimSpace(strings.ToLower(opt.Security))
	if opt.Security == "" {
		security = securityStartTLS
	} else if opt.Security == "starttls" {
		security = securityStartTLS
	} else if opt.Security == "tls" {
		security = securityTLS
	} else if opt.Security == "none" {
		security = securityNone
	} else {
		return nil, fmt.Errorf("invalid security: %s", opt.Security)
	}
	// port, check later for SSL
	if opt.Port == "" {
		if security == securityTLS {
			port = 993
		} else {
			port = 143
		}
	} else {
		port, err = strconv.Atoi(opt.Port)
		if err != nil {
			return nil, fmt.Errorf("invalid port value: %s", opt.Port)
		}
	}
	// host
	if opt.Host == "" {
		return nil, errors.New("host is required for IMAP")
	}
	// create filesystem
	f := &Fs{
		name:       name,
		root:       root,
		opt:        *opt,
		host:       opt.Host,
		port:       port,
		user:       user,
		pass:       pass,
		security:   security,
		skipVerify: opt.InsecureSkipVerify,
	}

	f.features = (&fs.Features{}).Fill(ctx, f)
	//
	return f, nil
}

// ------------------------------------------------------------
// mailclient functions
// ------------------------------------------------------------

type mailclient struct {
	conn      *client.Client
	f         *Fs
	delimiter string
	mailboxes []string
}

type mailreader struct {
	reader io.Reader
	size   int64
}

func newMailClient(f *Fs) (*mailclient, error) {
	cli := &mailclient{
		conn:      nil,
		f:         f,
		mailboxes: []string{},
	}
	// try and login
	err := cli.login()
	if err != nil {
		return nil, err
	}
	return cli, nil
}

func newMailReader(reader io.Reader, size int64) *mailreader {
	return &mailreader{
		reader: reader,
		size:   size,
	}
}

func (r *mailreader) Read(b []byte) (n int, err error) {
	return r.reader.Read(b)
}

func (r *mailreader) Len() int {
	return int(r.size)
}

func (m *mailclient) dirToMailbox(dir string) string {
	return strings.ReplaceAll(strings.Trim(dir, "/"), "/", m.delimiter)
}

func (m *mailclient) mailboxToDir(mailbox string) string {
	return strings.ReplaceAll(mailbox, m.delimiter, "/")
}

func (m *mailclient) login() error {
	var err error
	//
	address := fmt.Sprintf("%s:%d", m.f.host, m.f.port)
	// create options and options.TLSConfig()
	tlsConfig := new(tls.Config)
	// do we check for valid certificate
	tlsConfig.ServerName = m.f.host
	if m.f.skipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	// create client
	fs.Debugf(nil, "Connecting to %s", address)
	if m.f.security == securityStartTLS {
		m.conn, err = client.Dial(address)
		if err != nil {
			return fmt.Errorf("failed to dial IMAP server: %w", err)
		}
		// create STARTTLS client
		err = m.conn.StartTLS(tlsConfig)
		if err != nil {
			defer m.Logout()
			return err
		}
	} else if m.f.security == securityTLS {
		m.conn, err = client.DialTLS(address, nil)
		if err != nil {
			return fmt.Errorf("failed to dial IMAP server: %w", err)
		}
	} else if m.f.security == securityNone {
		m.conn, err = client.Dial(address)
		if err != nil {
			return fmt.Errorf("failed to dial IMAP server: %w", err)
		}
	}
	// connected ok, now login
	err = m.conn.Login(m.f.user, m.f.pass)
	if err != nil {
		defer m.Logout()
		return fmt.Errorf("failed to login: %w", err)
	}
	// get list of mailboxes
	err = m.RefreshMailboxes()
	if err != nil {
		defer m.Logout()
		return err
	}
	//
	return nil
}

func (m *mailclient) Logout() {
	if m.conn != nil {
		_ = m.conn.Logout()
	}
	//
	m.conn = nil
}

func (m *mailclient) RefreshMailboxes() error {
	if m.conn == nil {
		return fmt.Errorf("failed to get mailboxes : not connected")
	}
	// get mailboxes
	mailboxes := make(chan *imap.MailboxInfo, 10)
	done := make(chan error, 1)
	go func() {
		done <- m.conn.List("", "*", mailboxes)
	}()
	m.mailboxes = []string{}
	for mbox := range mailboxes {
		m.delimiter = mbox.Delimiter
		m.mailboxes = append(m.mailboxes, mbox.Name)
	}
	if err := <-done; err != nil {
		defer m.Logout()
		return fmt.Errorf("failed to get mailboxes: %v", err)
	}
	return nil
}

func (m *mailclient) ListMailboxes(dir string) ([]string, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("failed to list mailboxes : not connected")
	}
	list := []string{}
	for _, curr := range m.mailboxes {
		list = append(list, m.mailboxToDir(curr))
	}
	return getMatches(dir, list), nil
}

func (m *mailclient) HasMailbox(dir string) bool {
	if m.conn == nil {
		return false
	}
	for _, curr := range m.mailboxes {
		if m.dirToMailbox(dir) == curr {
			return true
		}
	}
	return false
}

func (m *mailclient) RenameMailbox(from string, to string) error {
	if m.conn == nil {
		return fmt.Errorf("failed to rename mailbox %s: not connected", from)
	}
	fs.Debugf(nil, "Rename mailbox %s to %s", from, to)
	err := m.conn.Rename(m.dirToMailbox(from), m.dirToMailbox(to))
	if err != nil {
		return fmt.Errorf("failed to rename mailbox %s: %w", from, err)
	}
	return m.RefreshMailboxes()
}

func (m *mailclient) CreateMailbox(name string) error {
	if m.conn == nil {
		return fmt.Errorf("failed to create mailbox %s: not connected", name)
	}
	fs.Debugf(nil, "Create mailbox %s", name)
	err := m.conn.Create(m.dirToMailbox(name))
	if err != nil {
		return fmt.Errorf("failed to create mailbox %s: %w", name, err)
	}
	return m.RefreshMailboxes()
}

func (m *mailclient) DeleteMailbox(name string) error {
	if m.conn == nil {
		return fmt.Errorf("failed to delete mailbox %s: not connected", name)
	} else if name == "" {
		return fmt.Errorf("cant remove root")
	}
	// select mailbox, readonly
	selectedMbox, err := m.conn.Select(m.dirToMailbox(name), true)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	if selectedMbox.Messages != 0 {
		return fmt.Errorf("mailbox not empty, has %d messages", selectedMbox.Messages)
	}
	//
	fs.Debugf(nil, "Delete mailbox %s", name)
	err = m.conn.Delete(m.dirToMailbox(name))
	if err != nil {
		return fmt.Errorf("failed to delete mailbox %s: %w", name, err)
	}
	return m.RefreshMailboxes()
}

func (m *mailclient) GetMailboxStatus(name string) (*imap.MailboxStatus, error) {
	if m.conn == nil {
		return nil, fmt.Errorf("failed to get message count for mailbox %s: not connected", name)
	} else if name == "" {
		return nil, nil
	}
	return m.conn.Status(m.dirToMailbox(name), []imap.StatusItem{imap.StatusMessages})
}

func (m *mailclient) ExpungeMailbox(mailbox string) error {
	if m.conn == nil {
		return fmt.Errorf("failed to expunge mailbox %s: not connected", mailbox)
	}
	// select mailbox, writable
	_, err := m.conn.Select(m.dirToMailbox(mailbox), false)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	// expunge
	fs.Debugf(nil, "Expunge mailbox: %s", mailbox)
	err = m.conn.Expunge(nil)
	if err != nil {
		return fmt.Errorf("failed to expunge mailbox: %w", err)
	}
	//
	return nil
}

func (m *mailclient) Save(mailbox string, date time.Time, size int64, reader io.Reader, flags []string) (err error) {
	if m.conn == nil {
		return fmt.Errorf("failed to save message to mailbox %s: not connected", mailbox)
	}
	fs.Debugf(nil, "Append message to mailbox %s", mailbox)
	err = m.conn.Append(m.dirToMailbox(mailbox), flags, date, newMailReader(reader, size))
	if err != nil {
		return fmt.Errorf("failed to save message to mailbox %s: %w", mailbox, err)
	}
	return nil
}

func (m *mailclient) SetFlags(mailbox string, seqset *imap.SeqSet, flags ...string) error {
	if m.conn == nil {
		return fmt.Errorf("failed to sert message flags: not connected")
	}
	// select mailbox, writable
	_, err := m.conn.Select(m.dirToMailbox(mailbox), false)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	// convert flags to interfaces
	flagInterfaces := make([]interface{}, len(flags))
	for i, v := range flags {
		flagInterfaces[i] = v
	}
	// Mark messages as deleted
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	fs.Debugf(nil, "Set flags for messages with ID [%s]: %s", seqset, strings.Fields(strings.Trim(fmt.Sprint(flags), "[]")))
	err = m.conn.Store(seqset, item, flagInterfaces, nil)
	if err != nil {
		return fmt.Errorf("failed to set flags: %w", err)
	}
	//
	return nil
}

func (m *mailclient) Fetch(mailbox string, seqset *imap.SeqSet, items []imap.FetchItem, action func(*imap.Message)) error {
	if m.conn == nil {
		return fmt.Errorf("failed to fetch messages : not connected")
	}
	// select mailbox, readonly
	selectedMbox, err := m.conn.Select(m.dirToMailbox(mailbox), true)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	if selectedMbox.Messages == 0 {
		return nil
	}
	//
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	// fetch messages on background
	go func() {
		done <- m.conn.Fetch(seqset, items, messages)
	}()
	// process messages
	for msg := range messages {
		if action != nil {
			action(msg)
		}
	}
	// check for error
	if err := <-done; err != nil {
		return fmt.Errorf("failed to fetch messages : %w", err)
	}
	return nil
}

func (m *mailclient) FetchSingle(mailbox string, id uint32, items []imap.FetchItem) (*imap.Message, error) {
	list := make([]*imap.Message, 0)
	seqset := new(imap.SeqSet)
	seqset.AddNum(id)
	// fetch messages
	err := m.Fetch(mailbox, seqset, items, func(msg *imap.Message) {
		list = append(list, msg)
	})

	if err != nil {
		return nil, err
	} else if len(list) == 0 {
		return nil, fmt.Errorf("failed to fetch message : not found")
	}
	return list[0], nil
}

func (m *mailclient) Search(mailbox string, criteria *imap.SearchCriteria) (seqNums []uint32, err error) {
	if m.conn == nil {
		return nil, fmt.Errorf("failed to search messages : not connected")
	}
	// select mailbox, readonly
	selectedMbox, err := m.conn.Select(m.dirToMailbox(mailbox), true)
	if err != nil {
		return nil, fs.ErrorDirNotFound
	}
	if selectedMbox.Messages == 0 {
		return []uint32{}, nil
	}
	//
	ids, err := m.conn.Search(criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search messages : %w", err)
	}
	return ids, err
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = &Fs{}
	_ fs.DirMover    = &Fs{}
	_ fs.PutStreamer = &Fs{}
	_ fs.Object      = &Object{}
	_ fs.MimeTyper   = &Object{}
)
