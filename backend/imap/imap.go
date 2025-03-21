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

func createHash(reader io.Reader, t ...hash.Type) (*hash.MultiHasher, error) {
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
	return hasher, nil
}

func matchesRoot(root string, value string) (bool, string) {
	a := strings.Split(strings.Trim(root, "/"), "/")
	b := strings.Split(strings.Trim(value, "/"), "/")
	count := len(a)
	//
	if strings.TrimSpace(a[0]) == "" && len(b) == 1 {
		// root is empty, return top if it has no children
		return true, b[0]
	} else if len(b) < len(a) {
		return false, ""
	} else if slices.Equal(a[0:count], b[0:count]) && len(b[count:]) == 1 {
		// value starts with root and has no children
		return true, b[count]
	}
	// value does not starts with root or has children
	return false, ""
}

func searchCriteriaFromName(name string) (*imap.SearchCriteria, error) {
	// extract info from file name
	info, err := parseMessageInfo(path.Base(name))
	// leave if not valid file name
	if err != nil {
		return nil, err
	}
	// init criteria for search
	criteria := imap.NewSearchCriteria()
	// include messages within date range (1 day before and after, imap uses date only no time)
	criteria.Since = info.ModTime().AddDate(0, -1, 0)
	criteria.Before = info.ModTime().AddDate(0, 1, 0)
	// include messages with size between size-50 and size+50
	criteria.Larger = uint32(info.Size() - 50)
	criteria.Smaller = uint32(info.Size() + 50)
	//
	return criteria, nil
}

func messageToObject(f *Fs, mailbox string, msg *imap.Message) (*Object, error) {
	var reader io.Reader
	// get body io.Reader (should be first value in msg.Body)
	for _, curr := range msg.Body {
		reader = curr
		break
	}
	if reader == nil {
		return nil, errorReadingMessage
	}
	//
	info, err := newMessageInfo(msg.InternalDate, reader, int64(msg.Size), msg.Flags)
	if err != nil {
		return nil, err
	}
	// return object
	return &Object{
		fs:      f,
		seqNum:  msg.SeqNum,
		mailbox: mailbox,
		info:    info,
		hashes:  map[hash.Type]string{hash.MD5: info.Checksum()},
	}, nil
}

func fetchEntries(f *Fs, dir string) (entries fs.DirEntries, err error) {
	var mailboxes []string
	var file string
	var criteria *imap.SearchCriteria
	// add root to dir
	dir = path.Join(f.root, dir)
	items := []imap.FetchItem{imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope, imap.FetchItem("BODY.PEEK[]")}
	seqset := new(imap.SeqSet)
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return nil, err
	}
	// connected, logout on exit
	defer client.Logout()
	// check if dir is a mailbox or a message
	if client.HasMailbox(dir) {
		// we are looking for mailbox contents
		fs.Debugf(nil, "List items in %s:%s", f.name, dir)
		file = ""
	} else {
		// we may be looking for a message
		dir, file = path.Split(dir)
		fs.Debugf(nil, "List items in %s:%s match %s", f.name, dir, file)
		criteria, err = searchCriteriaFromName(file)
		// file names must be in rclone maildir format
		if err != nil {
			// searching for filename not in maildir format, will never exist
			if err == errorInvalidFileName {
				return entries, nil
			}
			// different error
			return nil, err
		}
	}
	// get mailboxes
	mailboxes, err = client.ListMailboxes(dir)
	for _, name := range mailboxes {
		if file == "" || name == file {
			d := fs.NewDir(name, time.Unix(0, 0))
			entries = append(entries, d)
		}
	}
	// get message count
	messageCount, err := client.GetMessageCount(dir)
	if err != nil {
		return nil, err
	} else if messageCount == 0 {
		// mailbox is empty
		return entries, nil
	}
	// leave if dir is empty, root has no messages
	if criteria != nil {
		ids, err := client.Search(dir, criteria)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return entries, nil
		}
		seqset.AddNum(ids...)
		fs.Debugf(nil, "Fetch from %s with %d messages - IDS=%s", strings.Trim(dir, "/"), messageCount, strings.Join(strings.Fields(fmt.Sprint(ids)), ", "))
	} else {
		seqset.AddRange(1, messageCount)
		fs.Debugf(nil, "Fetch all from %s with %d messages", strings.Trim(dir, "/"), messageCount)
	}
	//
	err = client.Fetch(dir, seqset, items, func(mailbox string, msg *imap.Message) {
		o, err := messageToObject(f, mailbox, msg)
		if err != nil {
			fs.Debugf(nil, "Error converting message to object: %s", err.Error())
		} else if file == "" || o.Remote() == file {
			entries = append(entries, o)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch messages: %w", err)
	}
	return entries, nil
}

// ------------------------------------------------------------
// FileNameInfo
// ------------------------------------------------------------

type messageInfo struct {
	name   string
	date   time.Time
	md5sum string
	size   int64
	flags  []string
}

func newMessageInfo(internalDate time.Time, reader io.Reader, size int64, flags []string) (*messageInfo, error) {
	// calc md5 checksum
	hasher, err := createHash(reader, hash.MD5)
	if err != nil {
		return nil, errorReadingMessage
	}
	// get the hash
	md5sum, err := hasher.SumString(hash.MD5, false)
	if err != nil {
		return nil, errorReadingMessage
	}
	//
	return &messageInfo{
		date:   internalDate.UTC(),
		md5sum: md5sum,
		size:   size,
		flags:  flags,
	}, nil
}

func objectToMessageInfo(ctx context.Context, o fs.Object) (info *messageInfo, err error) {
	// open object
	reader, err := o.Open(ctx)
	if err != nil {
		return nil, errorReadingMessage
	}
	// check if a valid message
	_, err = mail.ReadMessage(reader)
	_ = reader.Close()
	if err != nil {
		return nil, errorInvalidMessage
	}
	// get md5 hash
	value, err := o.Hash(ctx, hash.MD5)
	if err == nil {
		return &messageInfo{
			name:   path.Base(o.Remote()),
			date:   o.ModTime(ctx).UTC(),
			md5sum: value,
			size:   o.Size(),
			flags:  []string{},
		}, nil
	}
	// get hash failed open again and call regular constructor
	reader, err = o.Open(ctx)
	if err != nil {
		return nil, errorReadingMessage
	}
	info, err = newMessageInfo(o.ModTime(ctx).UTC(), reader, o.Size(), []string{})
	info.name = o.Remote()
	_ = reader.Close()
	return info, err
}

func parseMessageInfo(name string) (*messageInfo, error) {
	matches := messageNameRegEx.FindStringSubmatch(name)
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
	md5sum := matches[2]
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
	return &messageInfo{
		date:   date,
		md5sum: md5sum,
		size:   size,
		flags:  flags,
	}, nil
}

func (i *messageInfo) Name() string {
	return i.name
}

func (i *messageInfo) MaildirName(flags bool) string {
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

func (i *messageInfo) ModTime() time.Time {
	return i.date
}

func (i *messageInfo) Checksum() string {
	return i.md5sum
}

func (i *messageInfo) Host() string {
	return imapHost
}

func (i *messageInfo) Size() int64 {
	return i.size
}

func (i *messageInfo) IsFlagSet(value string) bool {
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

func (f *Fs) findObject(ctx context.Context, remote string) (mailObject, error) {
	var info *messageInfo
	var searchName string
	var err error
	//
	searchName = path.Base(remote)
	srcObj, hasSource := ctx.Value(operations.SourceObjectKey).(fs.Object)
	if hasSource {
		// srcObj is valid, for message using date and checksum instead of name
		info, err = objectToMessageInfo(ctx, srcObj)
		if err == nil {
			searchName = info.MaildirName(false)
			fs.Debugf(nil, "SourceObjectKey found in context, Using %s for search", searchName)
		} else if err == errorInvalidMessage || err == errorReadingMessage {
			return nil, fserrors.NoRetryError(err)
		} else {
			fs.Debugf(nil, "SourceObjectKey found in context but unable to get information: %s", err.Error())
		}
	} else {
		// srcObj not set look using name
		info, err = parseMessageInfo(searchName)
		if err != nil {
			if err == errorInvalidFileName {
				return nil, fs.ErrorObjectNotFound
			}
			return nil, err
		}
	}
	entries, err := fetchEntries(f, searchName)
	if err != nil {
		return nil, err
	}
	// find message that matches hash
	for _, curr := range entries {
		o, isObject := curr.(*Object)
		if !isObject {
			fs.Debugf(nil, "not an object - %s", curr.Remote())
			continue
		} else if !o.Matches(info) {
			fs.Debugf(nil, "not a match - %s", curr.Remote())
			continue
		}
		// found set name to srcObj.Remote()
		// to fool copy functions so they think
		// the message is named like source
		if hasSource {
			o.info.name = path.Base(srcObj.Remote())
		}
		return o, nil
	}
	return nil, fs.ErrorObjectNotFound
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.findObject(ctx, remote)
}

// Put the object
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	var info *messageInfo
	var err error
	//
	srcObj, hasSource := ctx.Value(operations.SourceObjectKey).(fs.Object)
	if hasSource {
		// srcObj is valid, for message using date and checksum instead of name
		info, err = objectToMessageInfo(ctx, srcObj)
		if err == errorInvalidMessage || err == errorReadingMessage {
			return nil, fserrors.NoRetryError(err)
		} else if err != nil {
			fs.Debugf(nil, "Error converting object to messageInfo: %s", err.Error())
		}
	} else {
		// srcObj not set parse info from name
		info, err = parseMessageInfo(src.Remote())
		if err != nil {
			return nil, fserrors.NoRetryError(err)
		}
	}
	// connect to imap server
	client, err := newMailClient(f)
	if err != nil {
		return nil, fmt.Errorf("failed append: %w", err)
	}
	// connected, logout on exit
	defer client.Logout()
	// upload message
	err = client.Save(f.root, info.ModTime(), info.Size(), in, false)
	if err != nil {
		return nil, fmt.Errorf("failed append: %w", err)
	}
	//
	return f.findObject(ctx, info.MaildirName(false))
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

// Hashes are not supported
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
	fs      *Fs
	seqNum  uint32
	mailbox string
	info    *messageInfo
	hashes  map[hash.Type]string
}

type mailObject interface {
	fs.Object
	Matches(info *messageInfo) bool
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
	if o.info.Name() == "" {
		return o.info.MaildirName(true)
	}
	return o.info.Name()
}

// Hash returns the hash of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	// find hash in map
	value, ok := o.hashes[t]
	// hash found
	if ok {
		return value, nil
	}
	// get reader
	reader, err := o.Open(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to calculate %v: %w", t, err)
	}
	// create hash
	hasher, err := createHash(reader, t)
	if err != nil {
		return "", hash.ErrUnsupported
	}
	// get the hash
	value, err = hasher.SumString(t, false)
	if err != nil {
		return "", hash.ErrUnsupported
	}
	o.hashes[t] = value
	// return hash
	return value, err
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
	// connect to imap server
	client, err := newMailClient(o.fs)
	if err != nil {
		return nil, err
	}
	// connected, logout on exit
	defer client.Logout()
	// fetch message
	msg, err = client.FetchSingle(o.mailbox, o.seqNum, []imap.FetchItem{imap.FetchItem("BODY.PEEK[]")})
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
	err = client.Delete(o.mailbox, seqSet)
	if err != nil {
		return err
	}
	return nil
}

// Update an object
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	return fmt.Errorf("not supported")
}

// Matches compares object to a messageInfo
func (o *Object) Matches(info *messageInfo) bool {
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
	list := make([]string, 0)
	for _, curr := range m.mailboxes {
		matches, dirName := matchesRoot(dir, m.mailboxToDir(curr))
		if matches {
			list = append(list, dirName)
		}
	}
	return list, nil
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

func (m *mailclient) GetMessageCount(name string) (uint32, error) {
	if m.conn == nil {
		return 0, fmt.Errorf("failed to get message count for mailbox %s: not connected", name)
	} else if name == "" {
		return 0, nil
	}
	// select mailbox, readonly
	status, err := m.conn.Status(m.dirToMailbox(name), []imap.StatusItem{imap.StatusMessages})
	if err != nil {
		return 0, err
	}
	return status.Messages, nil
}

func (m *mailclient) GetMailbox() (string, error) {
	if m.conn == nil {
		return "", fmt.Errorf("failed to get current mailbox: not connected")
	}
	selectedMbox := m.conn.Mailbox()
	if selectedMbox == nil {
		return "", fmt.Errorf("failed to get current mailbox: none selected")
	}
	return m.mailboxToDir(selectedMbox.Name), nil
}

func (m *mailclient) Save(mailbox string, date time.Time, size int64, reader io.Reader, seen bool) (err error) {
	var flags []string
	//
	if m.conn == nil {
		return fmt.Errorf("failed to save message to mailbox %s: not connected", mailbox)
	}
	if seen {
		flags = []string{"\\Seen"}
	}
	//
	fs.Debugf(nil, "Append message to mailbox %s", mailbox)
	err = m.conn.Append(m.dirToMailbox(mailbox), flags, date, newMailReader(reader, size))
	if err != nil {
		return fmt.Errorf("failed to save message to mailbox %s: %w", mailbox, err)
	}
	return nil
}

func (m *mailclient) Delete(mailbox string, seqset *imap.SeqSet) error {
	if m.conn == nil {
		return fmt.Errorf("failed to delete messages: not connected")
	}
	// select mailbox, writable
	_, err := m.conn.Select(m.dirToMailbox(mailbox), false)
	if err != nil {
		return fs.ErrorDirNotFound
	}
	// Mark messages as deleted
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.DeletedFlag}
	fs.Debugf(nil, "Flag as deleted: %s", seqset)
	err = m.conn.Store(seqset, item, flags, nil)
	if err != nil {
		return fmt.Errorf("failed to delete messages: %w", err)
	}
	// expunge
	fs.Debugf(nil, "Expunge mailbox: %s", mailbox)
	err = m.conn.Expunge(nil)
	if err != nil {
		return fmt.Errorf("failed to delete messages: %w", err)
	}
	//
	return nil
}

func (m *mailclient) Fetch(mailbox string, seqset *imap.SeqSet, items []imap.FetchItem, action func(string, *imap.Message)) error {
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
			action(mailbox, msg)
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
	err := m.Fetch(mailbox, seqset, items, func(_ string, msg *imap.Message) {
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
)
