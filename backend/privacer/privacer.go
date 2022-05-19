// Package privacer provides wrappers for Fs and Object which split large files in chunks
//TODO: delayed deletion. on file or dir remove only metadata delete. remove files later by command. bonbon .trash folder
//			-- move to trash -- done
//			-- manage trash
//TODO: folders and files additional random id needed for delayed delete. -- done
//TODO: SetModTime -- done
//TODO: move -- done
//TODO: rename -- done
//TODO: remove -- done checking
//TODO: rmdir -- done checking
//TODO: copy
//TODO: remove file to trash after up -- done
//TODO: defer external sql function how to
//TODO: honor max chunk size
//TODO: copy file to mount. will it instant added to mysql or on write-back-delay? .. on write-back..nice
//TODO: optimize mysql querys
//TODO: add failed uploads to trash or delete direct

// copy or move is for file

package privacer

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	gohash "hash"
	"io"
	"math/rand"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"

	"database/sql"

	_ "github.com/go-sql-driver/mysql"
)

//
// Chunker's composite files have one or more chunks
// and optional metadata object. If it's present,
// meta object is named after the original file.
//
// The only supported metadata format is simplejson atm.
// It supports only per-file meta objects that are rudimentary,
// used mostly for consistency checks (lazily for performance reasons).
// Other formats can be developed that use an external meta store
// free of these limitations, but this needs some support from
// rclone core (e.g. metadata store interfaces).
//
// The following types of chunks are supported:
// data and control, active and temporary.
// Chunk type is identified by matching chunk file name
// based on the chunk name format configured by user and transaction
// style being used.
//
// Both data and control chunks can be either temporary (aka hidden)
// or active (non-temporary aka normal aka permanent).
// An operation creates temporary chunks while it runs.
// By completion it removes temporary and leaves active chunks.
//
// Temporary chunks have a special hardcoded suffix in addition
// to the configured name pattern.
// Temporary suffix includes so called transaction identifier
// (abbreviated as `xactID` below), a generic non-negative base-36 "number"
// used by parallel operations to share a composite object.
// Chunker also accepts the longer decimal temporary suffix (obsolete),
// which is transparently converted to the new format. In its maximum
// length of 13 decimals it makes a 7-digit base-36 number.
//
// When transactions is set to the norename style, data chunks will
// keep their temporary chunk names (with the transacion identifier
// suffix). To distinguish them from temporary chunks, the txn field
// of the metadata file is set to match the transaction identifier of
// the data chunks.
//
// Chunker can tell data chunks from control chunks by the characters
// located in the "hash placeholder" position of configured format.
// Data chunks have decimal digits there.
// Control chunks have in that position a short lowercase alphanumeric
// string (starting with a letter) prepended by underscore.
//
// Metadata format v1 does not define any control chunk types,
// they are currently ignored aka reserved.
// In future they can be used to implement resumable uploads etc.
//
const (
	ctrlTypeRegStr   = `[a-z][a-z0-9]{2,6}`
	tempSuffixFormat = `_%04s`
	tempSuffixRegStr = `_([0-9a-z]{4,9})`
	tempSuffixRegOld = `\.\.tmp_([0-9]{10,13})`
)

var (
	// regular expressions to validate control type and temporary suffix
	ctrlTypeRegexp   = regexp.MustCompile(`^` + ctrlTypeRegStr + `$`)
	tempSuffixRegexp = regexp.MustCompile(`^` + tempSuffixRegStr + `$`)
)

// Normally metadata is a small piece of JSON (about 100-300 bytes).
// The size of valid metadata must never exceed this limit.
// Current maximum provides a reasonable room for future extensions.
//
// Please refrain from increasing it, this can cause old rclone versions
// to fail, or worse, treat meta object as a normal file (see NewObject).
// If more room is needed please bump metadata version forcing previous
// releases to ask for upgrade, and offload extra info to a control chunk.
//
// And still chunker's primary function is to chunk large files
// rather than serve as a generic metadata container.
const maxMetadataSize = 1023
const maxMetadataSizeWritten = 255

// Current/highest supported metadata format.
const metadataVersion = 2

// optimizeFirstChunk enables the following optimization in the Put:
// If a single chunk is expected, put the first chunk using the
// base target name instead of a temporary name, thus avoiding
// extra rename operation.
// Warning: this optimization is not transaction safe.
const optimizeFirstChunk = false

// revealHidden is a stub until chunker lands the `reveal hidden` option.
const revealHidden = false

// Prevent memory overflow due to specially crafted chunk name
const maxSafeChunkNumber = 10000000

// Number of attempts to find unique transaction identifier
const maxTransactionProbes = 100

// standard chunker errors
var (
	ErrChunkOverflow = errors.New("chunk number overflow")
	ErrMetaTooBig    = errors.New("metadata is too big")
	ErrMetaUnknown   = errors.New("unknown metadata, please upgrade rclone")
)

// variants of baseMove's parameter delMode
const (
	delNever  = 0 // don't delete, just move
	delAlways = 1 // delete destination before moving
	delFailed = 2 // move, then delete and try again if failed
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "privacer",
		Description: "Transparently chunk/split large files",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "remote",
			Required: true,
			Help: `Remote to chunk/unchunk.

Normally should contain a ':' and a path, e.g. "myremote:path/to/dir",
"myremote:bucket" or maybe "myremote:" (not recommended).`,
		}, {
			Name:     "chunk_size",
			Advanced: false,
			Default:  fs.SizeSuffix(2147483648), // 2 GiB
			Help:     `Files larger than chunk size will be split in chunks.`,
		}, {
			Name:     "name_format",
			Advanced: true,
			Hide:     fs.OptionHideCommandLine,
			Default:  `*.rclone_chunk.###`,
			Help: `String format of chunk file names.

The two placeholders are: base file name (*) and chunk number (#...).
There must be one and only one asterisk and one or more consecutive hash characters.
If chunk number has less digits than the number of hashes, it is left-padded by zeros.
If there are more digits in the number, they are left as is.
Possible chunk files are ignored if their name does not match given format.`,
		}, {
			Name:     "start_from",
			Advanced: true,
			Hide:     fs.OptionHideCommandLine,
			Default:  1,
			Help: `Minimum valid chunk number. Usually 0 or 1.

By default chunk numbers start from 1.`,
		}, {
			Name:     "meta_format",
			Advanced: true,
			Hide:     fs.OptionHideCommandLine,
			Default:  "simplejson",
			Help: `Format of the metadata object or "none".

By default "simplejson".
Metadata is a small JSON file named after the composite file.`,
			Examples: []fs.OptionExample{{
				Value: "none",
				Help: `Do not use metadata files at all.
Requires hash type "none".`,
			}, {
				Value: "simplejson",
				Help: `Simple JSON supports hash sums and chunk validation.

It has the following fields: ver, size, nchunks, md5, sha1.`,
			}},
		}, {
			Name:     "hash_type",
			Advanced: false,
			Default:  "md5",
			Help: `Choose how chunker handles hash sums.

All modes but "none" require metadata.`,
			Examples: []fs.OptionExample{{
				Value: "none",
				Help: `Pass any hash supported by wrapped remote for non-chunked files.
Return nothing otherwise.`,
			}, {
				Value: "md5",
				Help:  `MD5 for composite files.`,
			}, {
				Value: "sha1",
				Help:  `SHA1 for composite files.`,
			}, {
				Value: "md5all",
				Help:  `MD5 for all files.`,
			}, {
				Value: "sha1all",
				Help:  `SHA1 for all files.`,
			}, {
				Value: "md5quick",
				Help: `Copying a file to chunker will request MD5 from the source.
Falling back to SHA1 if unsupported.`,
			}, {
				Value: "sha1quick",
				Help:  `Similar to "md5quick" but prefers SHA1 over MD5.`,
			}},
		}, {
			Name:     "fail_hard",
			Advanced: true,
			Default:  false,
			Help:     `Choose how chunker should handle files with missing or invalid chunks.`,
			Examples: []fs.OptionExample{
				{
					Value: "true",
					Help:  "Report errors and abort current command.",
				}, {
					Value: "false",
					Help:  "Warn user, skip incomplete file and proceed.",
				},
			},
		}, {
			Name:     "transactions",
			Advanced: true,
			Default:  "rename",
			Help:     `Choose how chunker should handle temporary files during transactions.`,
			Hide:     fs.OptionHideCommandLine,
			Examples: []fs.OptionExample{
				{
					Value: "rename",
					Help:  "Rename temporary files after a successful transaction.",
				}, {
					Value: "norename",
					Help: `Leave temporary file names and write transaction ID to metadata file.
Metadata is required for no rename transactions (meta format cannot be "none").
If you are using norename transactions you should be careful not to downgrade Rclone
as older versions of Rclone don't support this transaction style and will misinterpret
files manipulated by norename transactions.
This method is EXPERIMENTAL, don't use on production systems.`,
				}, {
					Value: "auto",
					Help: `Rename or norename will be used depending on capabilities of the backend.
If meta format is set to "none", rename transactions will always be used.
This method is EXPERIMENTAL, don't use on production systems.`,
				},
			},
		}},
	})
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, rpath string, m configmap.Mapper) (fs.Fs, error) {
	fs.Debugf("newfs", "%s", rpath)
	// chunks always chunk folder
	orpath := rpath
	//rpath = ""
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	if opt.StartFrom < 0 {
		return nil, errors.New("start_from must be non-negative")
	}

	remote := opt.Remote
	if strings.HasPrefix(remote, name+":") {
		return nil, errors.New("can't point remote at itself - check the value of the remote setting")
	}

	baseName, basePath, err := fspath.SplitFs(remote)
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote %q to wrap: %w", remote, err)
	}
	// Look for a file first
	remotePath := fspath.JoinRootPath(basePath, rpath)
	//baseFs, err := cache.Get(ctx, baseName+remotePath)
	rpath = basePath
	remotePath = rpath
	baseFs, err := cache.Get(ctx, baseName+remotePath)
	fs.Debugf("newfsstart", "1: %s 2: %s 3: %s", remotePath, baseFs, basePath)
	if err != fs.ErrorIsFile && err != nil {
		return nil, fmt.Errorf("failed to make remote %q to wrap: %w", baseName+remotePath, err)
	}
	if !operations.CanServerSideMove(baseFs) {
		return nil, errors.New("can't use chunker on a backend which doesn't support server-side move or copy")
	}

	f := &Fs{
		base:  baseFs,
		name:  name,
		root:  rpath,
		oroot: orpath,
		opt:   *opt,
	}
	cache.PinUntilFinalized(f.base, f)
	f.dirSort = true // processEntries requires that meta Objects prerun data chunks atm.

	if err := f.configure(opt.NameFormat, opt.MetaFormat, opt.HashType, opt.Transactions); err != nil {
		return nil, err
	}

	// Handle the tricky case detected by FsMkdir/FsPutFiles/FsIsFile
	// when `rpath` points to a composite multi-chunk file without metadata,
	// i.e. `rpath` does not exist in the wrapped remote, but chunker
	// detects a composite file because it finds the first chunk!
	// (yet can't satisfy fstest.CheckListing, will ignore)
	if err == nil && !f.useMeta && strings.Contains(rpath, "/") {
		firstChunkPath := f.makeChunkName(remotePath, 0, "", "")
		_, testErr := cache.Get(ctx, baseName+firstChunkPath)
		if testErr == fs.ErrorIsFile {
			err = testErr
		}
	}

	// Note 1: the features here are ones we could support, and they are
	// ANDed with the ones from wrappedFs.
	// Note 2: features.Fill() points features.PutStream to our PutStream,
	// but features.Mask() will nullify it if wrappedFs does not have it.
	f.features = (&fs.Features{
		CaseInsensitive:         true,
		DuplicateFiles:          true,
		ReadMimeType:            false, // Object.MimeType not supported
		WriteMimeType:           true,
		BucketBased:             true,
		CanHaveEmptyDirectories: true,
		ServerSideAcrossConfigs: true,
	}).Fill(ctx, f).Mask(ctx, baseFs).WrapsFs(f, baseFs)

	f.features.Disable("ListR") // Recursive listing may cause chunker skip files

	//trashRunner start
	c := cron.New()
	c.AddFunc("*/60 * * * * *", func() { trashRunnerStats(f) })
	c.AddFunc("* 60 * * * *", func() { trashRunnerDelete(f, ctx) })
	c.Start()

	return f, err
}

type mysqlEntry struct {
	ID      string `json:"id"`
	Parent  string `json:"parent"`
	Type    int    `json:"type"`
	Name    string `json:"name"`
	ModTime int64  `json:"modtime"`
	Md5     string `json:"md5"`
	Sha1    string `json:"sha1"`
	Size    int64  `json:"size"`
	Chunks  int    `json:"chunks"`
	CName   string `json:"cname"`
}

// Options defines the configuration for this backend
type Options struct {
	Remote       string        `config:"remote"`
	ChunkSize    fs.SizeSuffix `config:"chunk_size"`
	NameFormat   string        `config:"name_format"`
	StartFrom    int           `config:"start_from"`
	MetaFormat   string        `config:"meta_format"`
	HashType     string        `config:"hash_type"`
	FailHard     bool          `config:"fail_hard"`
	Transactions string        `config:"transactions"`
}

// Fs represents a wrapped fs.Fs
type Fs struct {
	name         string
	root         string
	oroot        string
	base         fs.Fs          // remote wrapped by chunker overlay
	wrapper      fs.Fs          // wrapper is used by SetWrapper
	useMeta      bool           // false if metadata format is 'none'
	useMD5       bool           // mutually exclusive with useSHA1
	useSHA1      bool           // mutually exclusive with useMD5
	hashFallback bool           // allows fallback from MD5 to SHA1 and vice versa
	hashAll      bool           // hash all files, mutually exclusive with hashFallback
	dataNameFmt  string         // name format of data chunks
	ctrlNameFmt  string         // name format of control chunks
	nameRegexp   *regexp.Regexp // regular expression to match chunk names

	opt         Options      // copy of Options
	features    *fs.Features // optional features
	dirSort     bool         // reserved for future, ignored
	useNoRename bool         // can be set with the transactions option
}

func (f *Fs) getRandomId() int64 {
	return time.Now().UnixNano()
}

// configure sets up chunker for given name format, meta format and hash type.
// It also seeds the source of random transaction identifiers.
// configure must be called only from NewFs or by unit tests.
func (f *Fs) configure(nameFormat, metaFormat, hashType, transactionMode string) error {
	if err := f.setHashType(hashType); err != nil {
		return err
	}

	return nil
}

// setHashType
// must be called *after* setMetaFormat.
//
// In the "All" mode chunker will force metadata on all files
// if the wrapped remote can't provide given hashsum.
func (f *Fs) setHashType(hashType string) error {
	fs.Debugf("setHashType", "start")

	f.useMD5 = false
	f.useSHA1 = false
	f.hashFallback = false
	f.hashAll = false
	requireMetaHash := true

	switch hashType {
	case "none":
		requireMetaHash = false
	case "md5":
		f.useMD5 = true
	case "sha1":
		f.useSHA1 = true
	case "md5quick":
		f.useMD5 = true
		f.hashFallback = true
	case "sha1quick":
		f.useSHA1 = true
		f.hashFallback = true
	case "md5all":
		f.useMD5 = true
		f.hashAll = !f.base.Hashes().Contains(hash.MD5) || f.base.Features().SlowHash
	case "sha1all":
		f.useSHA1 = true
		f.hashAll = !f.base.Hashes().Contains(hash.SHA1) || f.base.Features().SlowHash
	default:
		return fmt.Errorf("unsupported hash type '%s'", hashType)
	}
	if requireMetaHash && !f.useMeta {
		//return fmt.Errorf("hash type '%s' requires compatible meta format", hashType)
	}
	return nil
}

func trashRunnerStats(f *Fs) {
	fs.Debugf("trashRunner", "start")
	var trashInfoLine string

	trashCount := f.mysqlGetTrashCount()
	trashSize := f.mysqlGetTrashSize()

	trashInfoLine = fmt.Sprintf("(%s) Trashed Files. (%s) Total Size.",trashCount,trashSize)


	fs.Infof("trashRunner","%s",trashInfoLine)
	
}

func trashRunnerDelete(f *Fs, ctx context.Context) {
	fs.Debugf("trashRunnerDelete", "start")
	var _1s int64
	_1s = 1000000000
	_trashTime := time.Now().UnixNano() - (_1s * 1)

	_res,err := f.mysqlQuery("select * from trash where trashtime < ?",_trashTime)
	defer _res.Close()
	if err != nil {
		fs.Debugf("trashRunnerDelete","err %s",err)
		return
	}
	for _res.Next() {
		type _data struct {
			id int64
			Type int
			name string
			modtime int64
			md5 string
			sha1 string
			size int64
			chunks int
			cname string
			trashtime int64
		}

		var _record _data
		err := _res.Scan(&_record.id,&_record.Type,&_record.name,&_record.modtime,&_record.md5,&_record.sha1,&_record.size,&_record.chunks,&_record.cname,&_record.trashtime)
		if err != nil {
			fs.Debugf("trashRunnerDelete","err: %s",err)
			return
		}

			fs.Debugf("trashRunner","Delete: %s",_record.name)

			for i := 0; i < _record.chunks; i++ {
				_cname := f.makeSha1FromString(_record.cname + fmt.Sprint(i))[0:1]+"/"+_record.cname+"."+fmt.Sprint(i)
				fs.Infof("trashRunnerDelete","%s deleting chunk: %s",_record.name,_cname)
				_chunk , err := f.base.NewObject(ctx, _cname)
				if err != nil {
					fs.Infof("trashRunnerDelete","cant find chunk %s err: %s",_cname, err)
				} else {
					err := _chunk.Remove(ctx)
					if err !=nil {
						fs.Infof("trashRunnerDelete","cant delete chunk %s err: %s",_cname, err)
					}
				}
			}

			_del, err := f.mysqlQuery("delete from trash where id=?",_record.id)
			defer _del.Close()
			if err != nil {
				fs.Infof("trashRunnerDelete","error deleting:",_record.name)
			} else {
				fs.Infof("trashRunnerDelete","Removed %s from Trash. Freed: %s",_record.name,operations.SizeString(_record.size,true))
			}

	}

	
}

// setChunkNameFormat converts pattern based chunk name format
// into Printf format and Regular expressions for data and
// control chunks.
func (f *Fs) setChunkNameFormat(pattern string) error {
	// validate pattern
	if strings.Count(pattern, "*") != 1 {
		return errors.New("pattern must have exactly one asterisk (*)")
	}
	numDigits := strings.Count(pattern, "#")
	if numDigits < 1 {
		return errors.New("pattern must have a hash character (#)")
	}
	if strings.Index(pattern, "*") > strings.Index(pattern, "#") {
		return errors.New("asterisk (*) in pattern must come before hashes (#)")
	}
	if ok, _ := regexp.MatchString("^[^#]*[#]+[^#]*$", pattern); !ok {
		return errors.New("hashes (#) in pattern must be consecutive")
	}
	if dir, _ := path.Split(pattern); dir != "" {
		return errors.New("directory separator prohibited")
	}
	if pattern[0] != '*' {
		return errors.New("pattern must start with asterisk") // to be lifted later
	}

	// craft a unified regular expression for all types of chunks
	reHashes := regexp.MustCompile("[#]+")
	reDigits := "[0-9]+"
	if numDigits > 1 {
		reDigits = fmt.Sprintf("[0-9]{%d,}", numDigits)
	}
	reDataOrCtrl := fmt.Sprintf("(?:(%s)|_(%s))", reDigits, ctrlTypeRegStr)

	// this must be non-greedy or else it could eat up temporary suffix
	const mainNameRegStr = "(.+?)"

	strRegex := regexp.QuoteMeta(pattern)
	strRegex = reHashes.ReplaceAllLiteralString(strRegex, reDataOrCtrl)
	strRegex = strings.Replace(strRegex, "\\*", mainNameRegStr, -1)
	strRegex = fmt.Sprintf("^%s(?:%s|%s)?$", strRegex, tempSuffixRegStr, tempSuffixRegOld)
	f.nameRegexp = regexp.MustCompile(strRegex)

	// craft printf formats for active data/control chunks
	fmtDigits := "%d"
	if numDigits > 1 {
		fmtDigits = fmt.Sprintf("%%0%dd", numDigits)
	}
	strFmt := strings.Replace(pattern, "%", "%%", -1)
	strFmt = strings.Replace(strFmt, "*", "%s", 1)
	f.dataNameFmt = reHashes.ReplaceAllLiteralString(strFmt, fmtDigits)
	f.ctrlNameFmt = reHashes.ReplaceAllLiteralString(strFmt, "_%s")
	return nil
}

// build an sha1 string from input string
func (f *Fs) makeSha1FromString(input string) string {
	//return input
	h := sha1.New()
	h.Write([]byte(input))
	bs := h.Sum(nil)
	return hex.EncodeToString(bs)
}

// makeChunkName produces chunk name (or path) for a given file.
//
// filePath can be name, relative or absolute path of main file.
//
// chunkNo must be a zero based index of data chunk.
// Negative chunkNo e.g. -1 indicates a control chunk.
// ctrlType is type of control chunk (must be valid).
// ctrlType must be "" for data chunks.
//
// xactID is a transaction identifier. Empty xactID denotes active chunk,
// otherwise temporary chunk name is produced.
//
func (f *Fs) makeChunkName(filePath string, chunkNo int, ctrlType, xactID string) string {
	dir, parentName := path.Split(filePath)
	//fs.Debugf("makeChunkName","d: %s f: %s sum: %s",dir,parentName,f.makeSha1FromString(filePath))
	var name, tempSuffix string
	switch {
	case chunkNo >= 0 && ctrlType == "":
		name = fmt.Sprintf(f.dataNameFmt, parentName, chunkNo+f.opt.StartFrom)
	case chunkNo < 0 && ctrlTypeRegexp.MatchString(ctrlType):
		name = fmt.Sprintf(f.ctrlNameFmt, parentName, ctrlType)
	default:
		panic("makeChunkName: invalid argument") // must not produce something we can't consume
	}
	if xactID != "" {
		tempSuffix = fmt.Sprintf(tempSuffixFormat, xactID)
		if !tempSuffixRegexp.MatchString(tempSuffix) {
			panic("makeChunkName: invalid argument")
		}
	}

	name = f.makeSha1FromString(dir + name)
	fs.Debugf("makeChunkName", "%s", ".cache/"+name+tempSuffix)
	return ".cache/" + name + tempSuffix
}

// List the objects and directories in dir into entries.
// The entries can be returned in any order but should be
// for a complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't found.
//
// Commands normally cleanup all temporary chunks in case of a failure.
// However, if rclone dies unexpectedly, it can leave behind a bunch of
// hidden temporary chunks. List and its underlying chunkEntries()
// silently skip all temporary chunks in the directory. It's okay if
// they belong to an unfinished command running in parallel.
//
// However, there is no way to discover dead temporary chunks atm.
// As a workaround users can use `purge` to forcibly remove the whole
// directory together with dead chunks.
// In future a flag named like `--chunker-list-hidden` may be added to
// rclone that will tell List to reveal hidden chunks.
//

func (f *Fs) addSlash(path string) string {
	if path != "" {
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
	}
	return path
}

func (f *Fs) prefixSlash(path string) string {
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}
	return path
}

func (f *Fs) mysqlGetFileByPath(_path string) (obj *Object, error error) {
	_di, fi := path.Split(_path)
	_id := f.mysqlFindMyDirId(_di)
	_id = f.mysqlFindFileId(fi, _id)
	o, err := f.mysqlGetFile(_id)
	return o, err
}

func (f *Fs) mysqlGetFile(id string) (obj *Object, error error) {
	index, _ := f.mysqlQuery("select * from meta where type=2 and id=?", id)
	defer index.Close()
	index.Next()
	var entry mysqlEntry
	err := index.Scan(&entry.ID, &entry.Parent, &entry.Type, &entry.Name, &entry.ModTime, &entry.Md5, &entry.Sha1, &entry.Size, &entry.Chunks, &entry.CName)
	if err == nil {
		o := &Object{
			f:       f,
			remote:  entry.Name,
			modTime: time.Unix(0, entry.ModTime),
			size:    entry.Size,
			main:    nil,
			chunksC: entry.Chunks,
			chunks:  nil,
			md5:     entry.Md5,
			sha1:    entry.Sha1,
			mysqlID: entry.ID,
			cName:   entry.CName,
		}
		return o, nil
	} else {
		return nil, err
	}
}

func (f *Fs) mysqlFindDirId(dirName string, startId string) (id string) {
	fs.Debugf("mysqlFindDirId", "%s %s", dirName, startId)
	index, _ := f.mysqlQuery("select id,name from meta where type=1 and parent=?", startId)
	defer index.Close()
	for index.Next() {
		_id := ""
		_name := ""
		index.Scan(&_id, &_name)
		if _name == dirName {
			return _id
		}
	}
	return ""
}

func (f *Fs) mysqlFindFileId(fiName string, startId string) (id string) {
	fs.Debugf("mysqlFindFileId", "%s %s", fiName, startId)
	index, _ := f.mysqlQuery("select id,name from meta where type=2 and parent=?", startId)
	defer index.Close()
	for index.Next() {
		_id := ""
		_name := ""
		index.Scan(&_id, &_name)
		if _name == fiName {
			return _id
		}
	}
	return ""
}

func (f *Fs) mysqlFindMyDirId(dir string) (id string) {
	id = "0"
	dir = f.prefixSlash(f.cleanFolderSlashes(dir))
	if dir != "" {
		lo := strings.Split(dir, "/")
		for i := 1; i < len(lo); i++ {
			fs.Debugf("mysqlFindMyId", "find: %s", lo[i])
			//if i == 0 {
			id = f.mysqlFindDirId(lo[i], id)
			//} else {

			//}
		}
	}
	return
}

func (f *Fs) mysqlFindMyFileId(_path string) (id string) {
	di, fi := path.Split(_path)
	diId := f.mysqlFindMyDirId(di)
	fiId := f.mysqlFindFileId(fi, diId)
	return fiId
}

func (f *Fs) mysqlFindMyDirParentId(dir string) (id string) {
	di, _ := path.Split(dir)
	id = "0"
	dir = f.prefixSlash(f.cleanFolderSlashes(di))
	if dir != "" {
		lo := strings.Split(dir, "/")
		for i := 1; i < len(lo); i++ {
			fs.Debugf("mysqlFindMyId", "find: %s", lo[i])
			//if i == 0 {
			id = f.mysqlFindDirId(lo[i], id)
			//} else {

			//}
		}
	}
	return
}

func (f *Fs) mysqlGetTrashCount() string {
	_res, err := f.mysqlQuery("select count(*) from trash")
	defer _res.Close()
	if err == nil {
		_res.Next()
		var _count string
		_res.Scan(&_count)
		return _count
	} else {
		return "0"
	}
}

func (f *Fs) mysqlGetTrashSize() string {
	_res, err := f.mysqlQuery("select sum(size) from trash")
	defer _res.Close()
	if err == nil {
		_res.Next()
		var _count int64
		_res.Scan(&_count)
		return operations.SizeString(_count,true)
	} else {
		return "0B"
	}
}

func (f *Fs) mysqlBuildList(dir string, nroot string) (entries fs.DirEntries, err error) {
	entries = nil
	if f.mysqlIsFile(nroot) {
		fs.Debugf("mysqlBuildList", "nroot is file")
		di, _ := path.Split(nroot)
		nroot = di
	}
	fs.Debugf("mysqlBuildList", "start search at: %s", nroot)
	//finding
	// start = da39a3ee5e6b4b0d3255bfef95601890afd80709
	_id := "0"

	if nroot != "" {
		lo := strings.Split(nroot, "/")

		for i := 1; i < len(lo); i++ {
			fs.Debugf("mysqlBuildList", "find: %s", lo[i])
			//if i == 0 {
			_id = f.mysqlFindDirId(lo[i], _id)
			//} else {

			//}
		}
	}

	fs.Debugf("mysqlBuildList", "found my id: %s", _id)
	index, _ := f.mysqlQuery("select * from meta where parent=?", _id)
	defer index.Close()
	if err != nil {
		panic(err)
	}

	for index.Next() {
		var entry mysqlEntry
		err = index.Scan(&entry.ID, &entry.Parent, &entry.Type, &entry.Name, &entry.ModTime, &entry.Md5, &entry.Sha1, &entry.Size, &entry.Chunks, &entry.CName)
		if err != nil {
			panic(err.Error())
		}
		fs.Debugf("id", "%s", entry.ID)
		/* if entry.ID == "" {
			return nil, errors.New("dir not found")
		} */
		fs.Debugf("List", "type: %s name: %s parrent: %s", entry.Type, entry.Name, entry.Parent)
		// dir
		if entry.Type == 1 && entry.Name != "" {
			nentry := fs.NewDir(f.addSlash(dir)+entry.Name, time.Unix(0, entry.ModTime))
			nentry.SetID(entry.ID)
			entries = append(entries, nentry)
		}
		// file
		if entry.Type == 2 && entry.Name != "" {

			o := &Object{
				f:       f,
				remote:  f.addSlash(dir) + entry.Name,
				modTime: time.Unix(0, entry.ModTime),
				size:    entry.Size,
				main:    nil,
				chunksC: entry.Chunks,
				chunks:  nil,
				md5:     entry.Md5,
				sha1:    entry.Sha1,
				mysqlID: entry.ID,
				cName:   entry.CName,
			}

			//o.chunks = append(o.chunks, b1,b2,b3,b4)
			//nentry := fs.Object( f.addSlash(dir) + entry.Name, time.Unix(0,entry.ModTime))
			//b := f.newObject(o.remote,o,o.chunks)
			entries = append(entries, o)
		}
	}

	return
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf("List", "go: %s", dir)
	//entries, err = f.base.List(ctx, dir)
	//if err != nil {
	//	return nil, err
	//}
	//return f.processEntries(ctx, entries, dir)
	// 10.21.200.152

	entries = nil

	if dir != "" {
		// get parents
		//panic("list find parent")
		//dir =  dir + "/"
		//if !strings.HasPrefix(dir, "/") {
		//	dir = "/" + dir
		//}
	}

	root := path.Join(f.oroot, dir)
	//root := f.root
	nroot := root
	if root != "" && !strings.HasPrefix(root, "/") {
		nroot = "/" + root
	}
	if nroot == "/" {
		nroot = ""
	}
	/* if !strings.HasPrefix(root, "/") && root != "" {
		root = "/" + root
	}
	if root == "/" {
		root = ""
	} */
	//_, dname := path.Split(root)
	fs.Debugf("List", "dir: %s root: %s nroot: %s sum: %s", dir, f.oroot, nroot, f.makeSha1FromString(nroot))

	isdir := f.mysqlDirExists(nroot)
	isfile := f.mysqlIsFile(nroot)
	fs.Debugf("list", "isD: %s isF: %s", isdir, isfile)
	/* if !isdir && !isfile {
		return nil, nil
	} */

	/* if isfile {
		//TODO: error check
		entry, _ := f.mysqlGetFile(f.makeSha1FromString(nroot))
		entries = append(entries, entry)
		return
	} */

	//fs.Debugf("d","%s",index)
	//TODO: err check
	entries, _ = f.mysqlBuildList(dir, nroot)

	// file
	//entry, err := f.NewObject(ctx,"123")
	//entry1 := fs.NewDir("123",time.Now())
	//entries = append(entries, entry)
	//entries = append(entries, entry1)
	return

}

// ListR lists the objects and directories of the Fs starting
// from dir recursively into out.
//
// dir should be "" to start from the root, and should not
// have trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
//
// It should call callback for each tranche of entries read.
// These need not be returned in any particular order.  If
// callback returns an error then the listing will stop
// immediately.
//
// Don't implement this unless you have a more efficient way
// of listing recursively than doing a directory traversal.
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) (err error) {
	fs.Debugf("ListR", "go")
	return nil
}

// NewObject finds the Object at remote.
//
// Please note that every NewObject invocation will scan the whole directory.
// Using here something like fs.DirCache might improve performance
// (yet making the logic more complex).
//
// Note that chunker prefers analyzing file names rather than reading
// the content of meta object assuming that directory scans are fast
// but opening even a small file can be slow on some backends.
//
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf("NewObject", "run remote: %s", remote)
	ex := f.mysqlIsFile(remote)
	if ex {
		fs.Debugf("NewObject", " Found: %s", remote)
		_o, err := f.mysqlGetFileByPath(remote)
		return _o, err
	}
	fs.Debugf("NewObject", "not found: %s", remote)
	return nil, fs.ErrorObjectNotFound
}

// scanObject is like NewObject with optional quick scan mode.
// The quick mode avoids directory requests other than `List`,
// ignores non-chunked objects and skips chunk size checks.
func (f *Fs) scanObject(ctx context.Context, remote string, quickScan bool) (fs.Object, error) {
	fs.Debugf("scanObject", "run")
	return nil, nil
}

// put implements Put, PutStream, PutUnchecked, Update
func (f *Fs) put(
	ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options []fs.OpenOption,
	basePut putFn, action string, target fs.Object) (obj fs.Object, err error) {
	fs.Debugf("put", "srcro: %s srcre: %s remote: %s oroot: %s", src.Fs().Root(), src.Remote(), remote, f.oroot)
	fs.Debugf("root", "%s", f.oroot)
	fs.Debugf("muh", "%s %s", remote, f.oroot)
	//fs.Debugf("put","file %s",f.re)

	var metaObject fs.Object

	f.mysqlMkDirRe(f.oroot)

	// whe use our own structure
	//f.root=""

	c := f.newChunkingReader(src)

	/* defer func() {
		if err != nil {
			c.rollback(ctx, metaObject)
		}
	}() */

	// how to delete
	/* oldFsObject, err := f.NewObject(ctx, "muh.6")
	if err == nil {
		oldObject := oldFsObject.(*Object)
		err = oldObject.Remove(ctx)
	} */

	wrapIn := c.wrapStream(ctx, in, src)

	//body return check must be
	var min, max int64
	if c.sizeTotal <= 1024 {
		min = -1
		max = -1
	} else {
		s := c.sizeTotal / 2 / 2
		min = s
		max = c.sizeTotal - min
		//c.chunkSize = max
	}

	cname := fmt.Sprint(f.getRandomId())
	fs.Debugf("cname sha1", "%s", f.makeSha1FromString(cname))

	for c.chunkNo = 0; !c.done; c.chunkNo++ {
		//chunk size
		if min > 0 && max > 0 {
			rand.Seed(f.getRandomId())
			c.chunkLimit = rand.Int63n(max-min) + min
		}
		size := c.sizeLeft
		if size > c.chunkLimit {
			size = c.chunkLimit
		}

		info := f.wrapInfo(src, f.makeSha1FromString(cname + fmt.Sprint(c.chunkNo))[0:1]+"/"+cname+"."+fmt.Sprint(c.chunkNo), size)

		//FIXME: check for errors
		_, err := basePut(ctx, wrapIn, info, options...)
		if err != nil {
			//return nil, err
		}

		//c.updateHashes()
		//fs.Debugf("chunk hash","%s %s",c.md5,c.sha1)

		if c.sizeLeft == 0 && !c.done {
			// The file has been apparently put by hash, force completion.
			c.done = true
		}

	}
	/* if f.useMD5 {
		c.md5, _ = src.Hash(ctx,hash.MD5)
	}
	if f.useSHA1 {
		c.sha1, _ = src.Hash(ctx,hash.MD5)
	} */

	//md5, _ := src.Hash(ctx,hash.MD5)
	//sha1, _ := src.Hash(ctx,hash.SHA1)
	fs.Debugf("done", "d")
	/* info := f.wrapInfo(src, "123", c.sizeTotal)
	_, errChunk := basePut(ctx, wrapIn, info, options...)
	if errChunk != nil {
		return nil, errChunk
	} */
	c.updateHashes()
	o := &Object{
		remote:  remote,
		main:    metaObject,
		size:    c.sizeTotal,
		f:       f,
		md5:     c.md5,
		sha1:    c.sha1,
		chunksC: c.chunkNo,
		//chunks: chunks,
	}
	//o := f.newObject("", metaObject, c.chunks)

	// mount put: srcro:  srcre: 1/2/3/6.txt remote: 1/2/3/6.txt oroot:
	//			put new: di: 2/3/ fi: 123.txt
	// copy put: srcro: /tmp srcre: 123.txt remote: 123.txt oroot: 2/3
	// 			put new: di: 2/3/ fi: 123.txt
	_dst := path.Join(f.oroot, remote)
	di, fi := path.Split(_dst)
	//di = f.cleanFolderSlashes(di)
	fs.Debugf("put new", "di: %s fi: %s", di, fi)
	nid := fmt.Sprint(f.getRandomId()) //f.makeSha1FromString(f.prefixSlash(di+fi))
	pid := f.mysqlFindMyDirId(di)      //f.prefixSlash(f.cleanFolderSlashes(di))

	//fs.Debugf("muh1","%s",f.mysqlFindParentId(di))
	//fs.Debugf("muh2","%s",f.mysqlFindMyId(di))
	fs.Debugf("putid", "%s", nid)
	if f.mysqlIsFile(_dst) {
		fs.Debugf("put", "file exist move to trash")
		f.mysqlDeleteFile(_dst)
	}
	modT := src.ModTime(ctx).UnixNano()
	_r, _ := f.mysqlQuery("replace into meta (id,parent,type,name,modtime,md5,sha1,size,chunks,cname) values (?,?,?,?,?,?,?,?,?,?)", nid, pid, 2, fi, modT, o.md5, o.sha1, c.sizeTotal, c.chunkNo, cname)
	defer _r.Close()
	o.size = c.sizeTotal
	o.mysqlID = nid
	o.cName = cname
	o.modTime = src.ModTime(ctx)
	return o, nil

}

type putFn func(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error)

type chunkingReader struct {
	baseReader   io.Reader
	sizeTotal    int64
	sizeLeft     int64
	readCount    int64
	chunkSize    int64
	chunkLimit   int64
	chunkNo      int
	err          error
	done         bool
	chunks       []fs.Object
	expectSingle bool
	smallHead    []byte
	fs           *Fs
	hasher       gohash.Hash
	md5          string
	sha1         string
}

func (f *Fs) newChunkingReader(src fs.ObjectInfo) *chunkingReader {
	c := &chunkingReader{
		fs:        f,
		chunkSize: int64(f.opt.ChunkSize),
		sizeTotal: src.Size(),
	}
	c.chunkLimit = c.chunkSize
	c.sizeLeft = c.sizeTotal
	c.expectSingle = c.sizeTotal >= 0 && c.sizeTotal <= c.chunkSize
	return c
}

func (c *chunkingReader) wrapStream(ctx context.Context, in io.Reader, src fs.ObjectInfo) io.Reader {
	baseIn, wrapBack := accounting.UnWrap(in)

	switch {
	case c.fs.useMD5:
		srcObj := fs.UnWrapObjectInfo(src)
		if srcObj != nil && srcObj.Fs().Features().SlowHash {
			fs.Debugf("wrapStream", "using md5")
			fs.Debugf(src, "skip slow MD5 on source file, hashing in-transit")
			c.hasher = md5.New()
			break
		}
		if c.md5, _ = src.Hash(ctx, hash.MD5); c.md5 == "" {
			if c.fs.hashFallback {
				c.sha1, _ = src.Hash(ctx, hash.SHA1)
			} else {
				c.hasher = md5.New()
			}
		}
	case c.fs.useSHA1:
		srcObj := fs.UnWrapObjectInfo(src)
		if srcObj != nil && srcObj.Fs().Features().SlowHash {
			fs.Debugf("wrapStream", "using sha1")
			fs.Debugf(src, "skip slow SHA1 on source file, hashing in-transit")
			c.hasher = sha1.New()
			break
		}
		if c.sha1, _ = src.Hash(ctx, hash.SHA1); c.sha1 == "" {
			if c.fs.hashFallback {
				c.md5, _ = src.Hash(ctx, hash.MD5)
			} else {
				c.hasher = sha1.New()
			}
		}
	}

	if c.hasher != nil {
		fs.Debugf("wrapStream", "hasher not nil")
		baseIn = io.TeeReader(baseIn, c.hasher)
	}
	c.baseReader = baseIn
	return wrapBack(c)
}

func (c *chunkingReader) updateHashes() {
	if c.hasher == nil {
		return
	}
	switch {
	case c.fs.useMD5:
		c.md5 = hex.EncodeToString(c.hasher.Sum(nil))
	case c.fs.useSHA1:
		c.sha1 = hex.EncodeToString(c.hasher.Sum(nil))
	}
}

// Note: Read is not called if wrapped remote performs put by hash.
func (c *chunkingReader) Read(buf []byte) (bytesRead int, err error) {
	fs.Debugf("(c *chunkingReader) Read", "bytes: %s", bytesRead)
	if c.chunkLimit <= 0 {
		// Chunk complete - switch to next one.
		// Note #1:
		// We might not get here because some remotes (e.g. box multi-uploader)
		// read the specified size exactly and skip the concluding EOF Read.
		// Then a check in the put loop will kick in.
		// Note #2:
		// The crypt backend after receiving EOF here will call Read again
		// and we must insist on returning EOF, so we postpone refilling
		// chunkLimit to the main loop.
		return 0, io.EOF
	}
	if int64(len(buf)) > c.chunkLimit {
		buf = buf[0:c.chunkLimit]
	}
	bytesRead, err = c.baseReader.Read(buf)
	if err != nil && err != io.EOF {
		c.err = err
		c.done = true
		return
	}
	c.accountBytes(int64(bytesRead))
	if c.chunkNo == 0 && c.expectSingle && bytesRead > 0 && c.readCount <= maxMetadataSize {
		c.smallHead = append(c.smallHead, buf[:bytesRead]...)
	}
	if bytesRead == 0 && c.sizeLeft == 0 {
		err = io.EOF // Force EOF when no data left.
	}
	if err == io.EOF {
		c.done = true
	}
	return
}

func (c *chunkingReader) accountBytes(bytesRead int64) {
	c.readCount += bytesRead
	c.chunkLimit -= bytesRead
	if c.sizeLeft != -1 {
		c.sizeLeft -= bytesRead
	}
}

// dummyRead updates accounting, hashsums, etc. by simulating reads
func (c *chunkingReader) dummyRead(in io.Reader, size int64) error {
	if c.hasher == nil && c.readCount+size > maxMetadataSize {
		c.accountBytes(size)
		return nil
	}
	const bufLen = 1048576 // 1 MiB
	buf := make([]byte, bufLen)
	for size > 0 {
		n := size
		if n > bufLen {
			n = bufLen
		}
		if _, err := io.ReadFull(in, buf[0:n]); err != nil {
			return err
		}
		size -= n
	}
	return nil
}

// Put into the remote path with the given modTime and size.
//
// May create the object even if it returns an error - if so
// will return the object and the error, otherwise will return
// nil and the error
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf("Put", "start")
	return f.put(ctx, in, src, src.Remote(), options, f.base.Put, "put", nil)
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf("Purge", "start")
	return f.put(ctx, in, src, src.Remote(), options, f.base.Features().PutStream, "upload", nil)
}

// Update in to the object with the modTime given of the given size
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	basePut := o.f.base.Put
	if src.Size() < 0 {
		basePut = o.f.base.Features().PutStream
		if basePut == nil {
			return errors.New("wrapped file system does not support streaming uploads")
		}
	}
	oNew, err := o.f.put(ctx, in, src, o.Remote(), options, basePut, "update", o)
	if err == nil {
		*o = *oNew.(*Object)
	}
	return err
}

// PutUnchecked uploads the object
//
// This will create a duplicate if we upload a new file without
// checking to see if there is one already - use Put() for that.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf("PutUnchecked", "start")
	/* do := f.base.Features().PutUnchecked
	if do == nil {
		return nil, errors.New("can't PutUnchecked")
	}
	// TODO: handle range/limit options and really chunk stream here!
	o, err := do(ctx, in, f.wrapInfo(src, "", -1))
	if err != nil {
		return nil, err
	}
	return f.newObject("", o, nil), nil */
	return nil, nil
}

// Hashes returns the supported hash sets.
// Chunker advertises a hash type if and only if it can be calculated
// for files of any size, non-chunked or composite.
func (f *Fs) Hashes() hash.Set {
	// composites AND no fallback AND (chunker OR wrapped Fs will hash all non-chunked's)
	if f.useMD5 && !f.hashFallback && (f.hashAll || f.base.Hashes().Contains(hash.MD5)) {
		return hash.NewHashSet(hash.MD5)
	}
	if f.useSHA1 && !f.hashFallback && (f.hashAll || f.base.Hashes().Contains(hash.SHA1)) {
		return hash.NewHashSet(hash.SHA1)
	}
	return hash.NewHashSet() // can't provide strong guarantees
}

/* func (f *Fs) mysqlFindParent(path string) int64 {

} */

// Mkdir makes the directory (container, bucket)
//
// Shouldn't return an error if it already exists

func (f *Fs) cleanFolderSlashes(path string) string {
	if strings.HasPrefix(path, "/") {
		path = strings.TrimPrefix(path, "/")
	}
	if strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	return path
}

func (f *Fs) mysqlQuerySilence(_query string, args ...interface{}) (err error) {
	fs.Debugf("mysqlQuerySilence", "Query: %s a: %s", _query, args)
	db, err := sql.Open("mysql", "rclone:rclone@tcp(10.21.200.152:3306)/rclone")
	if err != nil {
		panic(err.Error())
	}
	defer db.Close()

	res, err := db.Query(_query, args...)
	fs.Debugf("mysqlQuerySilence", "err", err)
	//if err != nil {
	//    panic(err.Error())
	//}
	defer res.Close()
	return err
}

func (f *Fs) mysqlQuery(_query string, args ...interface{}) (res *sql.Rows, err error) {
	fs.Debugf("mysqlQuery", "Query: %s a: %s", _query, args)
	db, err := sql.Open("mysql", "rclone:rclone@tcp(10.21.200.152:3306)/rclone")
	if err != nil {
		panic(err.Error())
	}
	defer db.Close()

	res, err = db.Query(_query, args...)
	if err != nil {
		panic(err.Error())
	}
	//defer res.Close()
	return
}

func (f *Fs) mysqlDirExists(_path string) bool {
	// root
	if _path == "" || _path == "/" {
		return true
	}

	_r := f.mysqlFindMyDirId(_path)
	if _r == "" {
		return false
	} else {
		return true
	}
}

func (f *Fs) mysqlChangeDirParent(idFrom string, idTo string) (err error) {
	index, err := f.mysqlQuery("update meta set id=? where id=?", idFrom, idTo)
	defer index.Close()
	return err
}

func (f *Fs) mysqlDirIsEmpty(id string) bool {
	// root
	/* if path == "" {
		return false
	} */
	index, _ := f.mysqlQuery("select id from meta where parent=? limit 1", id)
	defer index.Close()
	//index.Next()
	//count, _ := index.Columns()
	state := index.Next()
	if state == false {
		fs.Debugf("mysqlDirIsEmpty", "true")
		return true
	} else {
		fs.Debugf("mysqlDirIsEmpty", "false")
		return false
	}
}

func (f *Fs) mysqlIsFile(_path string) bool {
	if _path == "" {
		return false
	}
	di, fi := path.Split(_path)
	pid := f.mysqlFindMyDirId(di)
	fs.Debugf("mysqlIsFile", "pid: %s", pid)
	index, _ := f.mysqlQuery("select * from meta where type=2 and name=? and parent=?", fi, pid)
	defer index.Close()
	//index.Next()
	//count, _ := index.Columns()
	state := index.Next()
	fs.Debugf("mysqlIsFile", "state: %s", state)
	return state
}

func (f *Fs) mysqlIsFileByID(id string) bool {
	index, _ := f.mysqlQuery("select * from meta where type=2 and id=?", id)
	defer index.Close()
	//index.Next()
	//count, _ := index.Columns()
	state := index.Next()
	fs.Debugf("mysqlIsFileByID", "%s", state)
	return state
}

func (f *Fs) mysqlUpdateTime(id string, mod int64) (err error) {
	index, err := f.mysqlQuery("update meta set modtime=? where id=?", mod, id)
	defer index.Close()
	return err

}

func (f *Fs) mysqlQueryCount(_query string, args interface{}) (res int, err error) {
	db, err := sql.Open("mysql", "rclone:rclone@tcp(10.21.200.152:3306)/rclone")
	if err != nil {
		panic(err.Error())
	}
	defer db.Close()

	err = db.QueryRow(_query, args).Scan(&res)
	if err != nil {
		panic(err.Error())
	}
	//defer res.Close()
	return
}

/* func (f *Fs) mysqlGetFolderID(_path string) int {
	if _path == "" {
		return 1
	}
	root := strings.Split(f.cleanFolderSlashes(_path),"/")
	//parent := 0
	for _, s := range root {
		f.mysqlQuery("select parent from meta ?", s)
	}



} */

func (f *Fs) mysqlMkDirRe(_path string) {

	_path = f.cleanFolderSlashes(_path)
	fs.Debugf("mysqlMkDirRe", "start: %s", _path)
	root := strings.Split(_path, "/")
	if _path == "" {
		return
	}
	//root = append([]string{""},root...)
	//dname, _ := path.Split(_path)
	_p := ""
	_pid := "0"
	for _, name := range root {
		_pnew := f.mysqlFindDirId(name, _pid)
		fs.Debugf("1. mysqlMkDirRe", "_pnew: %s _pid: %s name: %s", _pnew, _pid, name)
		_nid := ""
		if _pnew == "" {
			_nid = fmt.Sprint(f.getRandomId())
			fs.Debugf("2. mysqlMkDirRe in", "_pnew: %s _pid: %s name: %s", _pnew, _pid, name)
			//fs.Debugf("test","%s",time.Now().UnixMilli())
			//_r, _ := f.mysqlQuery("insert ignore into meta (id, parent, type, name, modtime) values(?, ?, ?, ? ,?)",f.makeSha1FromString(_p+"/"+name), f.makeSha1FromString(_p), 1, name, time.Now().UnixNano())
			_r, _ := f.mysqlQuery("insert ignore into meta (id, parent, type, name, modtime) values(?, ?, ?, ? ,?)", _nid, _pid, 1, name, time.Now().UnixNano())
			defer _r.Close()
		}
		_p += "/" + name

		_pid = f.mysqlFindMyDirId(_p)
	}
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {

	root := path.Join(f.oroot, dir)
	//_, dname := path.Split(root)

	fs.Debugf("MKdir", "go dir: %s f.oroot: %s root: %s", dir, f.oroot, root)

	if root == "" {
		// no folder, silence discard
		return nil
	}

	f.mysqlMkDirRe(root)

	return nil
}

// Rmdir removes the directory (container, bucket) if empty
//
// Return an error if it doesn't exist or isn't empty

func (f *Fs) mysqlRmDir(path string) (err error) {

	return nil
}

func (f *Fs) mysqlRmDirByID(id string) (err error) {
	_r, err := f.mysqlQuery("delete from meta where type=1 and id=?", id)
	defer _r.Close()
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Debugf("RmDir", "run %s oroot: %s", dir, f.oroot)
	_id := f.mysqlFindMyDirId(dir)
	if f.mysqlDirIsEmpty(_id) {
		f.mysqlRmDirByID(_id)
	} else {
		return fs.ErrorDirectoryNotEmpty
	}
	return nil
}

// Purge all files in the directory
//
// Implement this if you have a way of deleting all the files
// quicker than just running Remove() on the result of List()
//
// Return an error if it doesn't exist.
//
// This command will chain to `purge` from wrapped remote.
// As a result it removes not only composite chunker files with their
// active chunks but also all hidden temporary chunks in the directory.
//
func (f *Fs) Purge(ctx context.Context, dir string) error {
	fs.Debugf("Purge", "start dir: %s oroot: %s", dir, f.oroot)
	/* do := f.base.Features().Purge
	if do == nil {
		return fs.ErrorCantPurge
	}
	return do(ctx, dir) */
	return fs.ErrorCantPurge
}

// Remove an object (chunks and metadata, if any)
//
// Remove deletes only active chunks of the composite object.
// It does not try to look for temporary chunks because they could belong
// to another command modifying this composite file in parallel.
//
// Commands normally cleanup all temporary chunks in case of a failure.
// However, if rclone dies unexpectedly, it can leave hidden temporary
// chunks, which cannot be discovered using the `list` command.
// Remove does not try to search for such chunks or to delete them.
// Sometimes this can lead to strange results e.g. when `list` shows that
// directory is empty but `rmdir` refuses to remove it because on the
// level of wrapped remote it's actually *not* empty.
// As a workaround users can use `purge` to forcibly remove it.
//
// In future, a flag `--chunker-delete-hidden` may be added which tells
// Remove to search directory for hidden chunks and remove them too
// (at the risk of breaking parallel commands).
//
// Remove is the only operation allowed on the composite files with
// invalid or future metadata format.
// We don't let user copy/move/update unsupported composite files.
// Let's at least let her get rid of them, just complain loudly.
//
// This can litter directory with orphan chunks of unsupported types,
// but as long as we remove meta object, even future releases will
// treat the composite file as removed and refuse to act upon it.
//
// Disclaimer: corruption can still happen if unsupported file is removed
// and then recreated with the same name.
// Unsupported control chunks will get re-picked by a more recent
// rclone version with unexpected results. This can be helped by
// the `delete hidden` flag above or at least the user has been warned.
//
func (o *Object) Remove(ctx context.Context) (err error) {
	fs.Debugf("Remove", "run id: %s", o.remote)
	o.f.mysqlDeleteFile(o.remote)

	return nil
}

func (f *Fs) mysqlDeleteFile(_path string) (err error) {
	//TODO: error check
	//INSERT INTO table2 select * from table1 where ts < date_sub(@N,INTERVAL 32 DAY);
	//DELETE FROM table1 WHERE ts < date_sub(@N,INTERVAL 32 DAY);
	//_r, err :=f.mysqlQuery("insert into trash (id,parent,type,name,modtime,md5,sha1,size,chunks,cname) select * from meta where id=?", id)
	//defer _r.Close()
	di, fi := path.Split(_path)

	_did := f.mysqlFindMyDirId(di)

	_fid := f.mysqlFindFileId(fi, _did)

	o, _ := f.mysqlGetFile(_fid)

	_r, err := f.mysqlQuery("delete from meta where id=?", _fid)
	defer _r.Close()

	o.mysqlID = fmt.Sprint(f.getRandomId())

	_r, err = f.mysqlQuery("insert into trash (id,type,name,modtime,md5,sha1,size,chunks,cname,trashtime) values(?,?,?,?,?,?,?,?,?,?)", o.mysqlID, 2, o.remote, o.modTime.UnixNano(), o.md5, o.sha1, o.size, o.chunksC, o.cName, o.mysqlID)
	defer _r.Close()
	return nil

}

// copyOrMove implements copy or move
func (f *Fs) copyOrMove(ctx context.Context, o *Object, remote string, do copyMoveFn, md5, sha1, opName string) (fs.Object, error) {
	fs.Debugf("copyOrMove", "run")
	return nil, nil
}

type copyMoveFn func(context.Context, fs.Object, string) (fs.Object, error)

// Copy src to this remote using server-side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf("Copy", "start src: %s dst: %s", src.Remote(), remote)
	//return nil, fs.ErrorCantCopy //fs.ErrorCantCopy

	// when move fail let rclone copy it self
	return nil, fs.ErrorCantCopy
}

// Move src to this remote using server-side move operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf("Move", "go remote: %s src: %s", remote, src.Remote())
	//return nil, fs.ErrorCantMove
	if f.mysqlIsFile(remote) {
		return nil, fs.ErrorDirExists
	}
	di, _ := path.Split(remote)
	_dstDiEx := f.mysqlDirExists(di)
	if !_dstDiEx {
		return nil, fs.ErrorDirNotFound
	}

	_dstDiId := f.mysqlFindMyDirId(di)
	_dstFiId := f.mysqlFindMyFileId(src.Remote())

	fs.Debugf("Move", "_dstDiId: %s _dstFiId: %s", _dstDiId, _dstFiId)

	err := f.mysqlMoveFile(_dstFiId, _dstDiId)

	o, err := f.mysqlGetFileByPath(remote)

	return o, err
}

func (f *Fs) mysqlMoveDir(id string, dstDirId string) (err error) {
	index, err := f.mysqlQuery("update meta set parent=? where id=?", dstDirId, id)
	defer index.Close()
	fs.Debugf("mysqlMoveDir", "err", err)
	return err

}

func (f *Fs) mysqlMoveFile(id string, dstDirId string) (err error) {
	index, err := f.mysqlQuery("update meta set parent=? where id=?", dstDirId, id)
	defer index.Close()
	fs.Debugf("mysqlMoveDir", "err", err)
	return err

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
	fs.Debugf("DirMove", "start src: %s srcR: %s dstR: %s", src, srcRemote, dstRemote)

	_idSrc := f.mysqlFindMyDirId(srcRemote)
	_idDst := f.mysqlFindMyDirId(dstRemote)

	_di, _ := path.Split(dstRemote)

	_idDstP := f.mysqlFindMyDirId(_di)

	fs.Debugf("DirMove", "idSrc: %s idDst: %s idDstP: %s", _idSrc, _idDst, _idDstP)

	// better than moving
	if _idDst != "" {
		return fs.ErrorDirExists
	}
	if _idDstP == "" {
		return fs.ErrorDirNotFound
	}

	//f.mysqlMkDirRe(dstRemote)

	_idDst = f.mysqlFindMyDirId(dstRemote)

	f.mysqlMoveDir(_idSrc, _idDstP)
	return nil
}

// CleanUp the trash in the Fs
//
// Implement this if you have a way of emptying the trash or
// otherwise cleaning up old versions of files.
func (f *Fs) CleanUp(ctx context.Context) error {
	do := f.base.Features().CleanUp
	if do == nil {
		return errors.New("can't CleanUp")
	}
	return do(ctx)
}

// About gets quota information from the Fs
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	do := f.base.Features().About
	if do == nil {
		return nil, errors.New("About not supported")
	}
	return do(ctx)
}

// UnWrap returns the Fs that this Fs is wrapping
func (f *Fs) UnWrap() fs.Fs {
	return f.base
}

// WrapFs returns the Fs that is wrapping this Fs
func (f *Fs) WrapFs() fs.Fs {
	return f.wrapper
}

// SetWrapper sets the Fs that is wrapping this Fs
func (f *Fs) SetWrapper(wrapper fs.Fs) {
	f.wrapper = wrapper
}

// ChangeNotify calls the passed function with a path
// that has had changes. If the implementation
// uses polling, it should adhere to the given interval.
//
// Replace data chunk names by the name of composite file.
// Ignore temporary and control chunks.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	fs.Debugf("ChangeNotify", "run")
}

// Shutdown the backend, closing any background tasks and any
// cached connections.
func (f *Fs) Shutdown(ctx context.Context) error {
	do := f.base.Features().Shutdown
	if do == nil {
		return nil
	}
	return do(ctx)
}

// Object represents a composite file wrapping one or more data chunks
type Object struct {
	remote    string
	main      fs.Object   // meta object if file is composite, or wrapped non-chunked file, nil if meta format is 'none'
	chunks    []fs.Object // active data chunks if file is composite, or wrapped file as a single chunk if meta format is 'none'
	chunksC   int         // chunk count
	cName     string      // chunk remote name
	size      int64       // cached total size of chunks in a composite file or -1 for non-chunked files
	isFull    bool        // true if metadata has been read
	xIDCached bool        // true if xactID has been read
	unsure    bool        // true if need to read metadata to detect object type
	xactID    string      // transaction ID for "norename" or empty string for "renamed" chunks
	md5       string
	sha1      string
	modTime   time.Time
	mysqlID   string
	f         *Fs
}

// Fs returns read only access to the Fs that this object is part of
func (o *Object) Fs() fs.Info {
	return o.f
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	fs.Debugf("size", "run")
	/* if o.isComposite() {
		return o.size // total size of data chunks in a composite file
	}
	return o.mainChunk().Size() // size of wrapped non-chunked file */
	return o.size
}

// Storable returns whether object is storable
func (o *Object) Storable() bool {
	fs.Debugf("storable", "run")
	return true
}

// ModTime returns the modification time of the file
func (o *Object) ModTime(ctx context.Context) time.Time {
	fs.Debugf("modtime", "run %s", o.modTime)
	//return o.mainChunk().ModTime(ctx)

	return o.modTime
}

// SetModTime sets the modification time of the file
func (o *Object) SetModTime(ctx context.Context, mtime time.Time) error {
	fs.Debugf("setmodtime", "run: new: %s obj: %s", mtime, o.modTime)
	o.f.mysqlUpdateTime(o.mysqlID, mtime.UnixNano())
	o.modTime = mtime

	fs.Debugf("setmodtime", "changed: new: %s obj: %s", mtime, o.modTime)
	return nil
	//return o.mainChunk().SetModTime(ctx, mtime)
}

// Hash returns the selected checksum of the file.
// If no checksum is available it returns "".
//
// Hash won't fail with `unsupported` error but return empty
// hash string if a particular hashsum type is not supported
//
// Hash takes hashsum from metadata if available or requests it
// from wrapped remote for non-chunked files.
// Metadata (if meta format is not 'none') is by default kept
// only for composite files. In the "All" hashing mode chunker
// will force metadata on all files if particular hashsum type
// is not supported by wrapped remote.
//
// Note that Hash prefers the wrapped hashsum for non-chunked
// file, then tries to read it from metadata. This in theory
// handles the unusual case when a small file has been tampered
// on the level of wrapped remote but chunker is unaware of that.
//
func (o *Object) Hash(ctx context.Context, hashType hash.Type) (string, error) {
	fs.Debugf("Hash", "run md5: %s sha1: %s", o.md5, o.sha1)
	//FIXME: hash
	//return "", nil
	//return o.Hash(ctx,hashType)
	switch hashType {
	case hash.MD5:
		if o.md5 == "" {
			return "", nil
		}
		return o.md5, nil
	case hash.SHA1:
		if o.sha1 == "" {
			return "", nil
		}
		return o.sha1, nil
	default:
		return "", hash.ErrUnsupported
	}
	/* fs.Debugf("hash","run")
	if err := o.readMetadata(ctx); err != nil {
		return "", err // valid metadata is required to get hash, abort
	}
	if !o.isComposite() {
		// First, chain to the wrapped non-chunked file if possible.
		if value, err := o.mainChunk().Hash(ctx, hashType); err == nil && value != "" {
			return value, nil
		}
	}

	// Try hash from metadata if the file is composite or if wrapped remote fails.
	switch hashType {
	case hash.MD5:
		if o.md5 == "" {
			return "", nil
		}
		return o.md5, nil
	case hash.SHA1:
		if o.sha1 == "" {
			return "", nil
		}
		return o.sha1, nil
	default:
		return "", hash.ErrUnsupported
	} */
}

// Open opens the file for read.  Call Close() on the returned io.ReadCloser
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (rc io.ReadCloser, err error) {
	fs.Debugf("open", "run remote: %s %s", o.Remote(), o.chunks)
	//f.makeSha1FromString(cname+fmt.Sprint(c.chunkNo))[0:1]
	//return o.Open(ctx, options...)
	if o.chunks == nil {
		fs.Debugf("open", "chunks nil count: %s", o.chunksC)
		if o.chunksC <= 0 {
			panic("chunks missing")
		}
		for i := 0; i < o.chunksC; i++ {
			cn := o.f.makeSha1FromString(o.cName + fmt.Sprint(i))[0:1] + "/" + o.cName + "." + fmt.Sprint(i)
			o.f.root = ""
			fs.Debugf("new chunk", "%s", cn)

			//t,_ := NewFs(ctx,"privacer",":",nil)
			//chunk, err :=t.NewObject(ctx,cn)
			//buf, err := o.f.base.List(ctx,"")
			//fs.Debugf("buf","%s",buf)
			c, err := cache.Get(ctx, o.f.opt.Remote)
			chunk, err := c.NewObject(ctx, cn)
			////chunk, err  := o.f.base.NewObject(ctx, "//"+cn)
			//chunk, err  := o.f.NewObject(ctx, cn)
			//chunk1, err  :=chunk1.Fs().base.NewObject(ctx, cn)
			//chunk, err := b.
			if err != nil {
				panic(err)
			}
			//fs.Debugf("nfs","name: %s",o.f.name)
			//nfs, _ := fs.NewFs(nil,o.f.name+":")
			//chunk ,_ :=nfs.NewObject(ctx, cn)
			o.chunks = append(o.chunks, chunk)

		}
	}
	var openOptions []fs.OpenOption
	var offset, limit int64 = 0, -1

	for _, option := range options {
		switch opt := option.(type) {
		case *fs.SeekOption:
			fs.Debugf("Open", "seekOption opt.Offset: %s", opt.Offset)
			offset = opt.Offset
		case *fs.RangeOption:
			fs.Debugf("open", "range o.size: %s remote: %s", o.size, o.remote)
			offset, limit = opt.Decode(o.size)
		default:
			// pass Options on to the wrapped open, if appropriate
			fs.Debugf("Open", "default %s", option)
			openOptions = append(openOptions, option)
		}
	}

	if offset < 0 {
		return nil, errors.New("invalid offset")
	}
	if limit < 0 {
		fs.Debugf("Open", "size: %s", o.size-offset)
		limit = o.size - offset
	}

	return o.newLinearReader(ctx, offset, limit, openOptions)

}

// linearReader opens and reads file chunks sequentially, without read-ahead
type linearReader struct {
	ctx     context.Context
	chunks  []fs.Object
	options []fs.OpenOption
	limit   int64
	count   int64
	pos     int
	reader  io.ReadCloser
	err     error
}

func (o *Object) newLinearReader(ctx context.Context, offset, limit int64, options []fs.OpenOption) (io.ReadCloser, error) {

	r := &linearReader{
		ctx:     ctx,
		chunks:  o.chunks,
		options: options,
		limit:   limit,
	}
	fs.Debugf("newLinearReader", "run %s %s", o.remote, len(r.chunks))
	// skip to chunk for given offset
	err := io.EOF
	for offset >= 0 && err != nil {
		fs.Debugf("newLinearReader", "nextChunk")
		offset, err = r.nextChunk(offset)
		fs.Debugf("newLinearReader", "nextChunk offset: %s err: %s", offset, err)
	}

	if err == nil || err == io.EOF {
		//panic((""))
		r.err = err
		return r, nil
	}

	return nil, err
}

func (r *linearReader) nextChunk(offset int64) (int64, error) {
	fs.Debugf("(r *linearReader) nextChunk", "run offset: %s r.pos: %s lenChunks: %s", offset, r.pos, len(r.chunks))

	if r.err != nil {
		return -1, r.err
	}
	if r.pos >= len(r.chunks) || r.limit <= 0 || offset < 0 {
		fs.Debugf("nextChunk", "break %s %s %s", len(r.chunks), r.limit, offset)
		return -1, io.EOF
	}

	chunk := r.chunks[r.pos]
	count := chunk.Size()
	fs.Debugf("nextChunk", "chunk: %s count: %s", chunk, count)
	r.pos++

	if offset >= count {
		return offset - count, io.EOF
	}
	count -= offset
	if r.limit < count {
		count = r.limit
	}
	options := append(r.options, &fs.RangeOption{Start: offset, End: offset + count - 1})

	if err := r.Close(); err != nil {
		return -1, err
	}

	reader, err := chunk.Open(r.ctx, options...)
	fs.Debugf("nectChunk", "chunk.Open")
	if err != nil {
		return -1, err
	}

	r.reader = reader
	r.count = count
	return offset, nil
}

func (r *linearReader) Read(p []byte) (n int, err error) {
	fs.Debugf("(r *linearReader) Read", "run count: %s limit: %s pos: %s", r.count, r.limit, r.pos)
	//panic("")
	if r.err != nil {
		fs.Debugf("(r *linearReader) Read", "err: %s", r.err)
		//panic(r.pos)
		return 0, r.err
	}

	if r.limit <= 0 {
		r.err = io.EOF
		return 0, io.EOF
	}
	fs.Debugf("123", "%s", r.count)
	for r.count <= 0 {

		// current chunk has been read completely or its size is zero
		off, err := r.nextChunk(0)
		if off < 0 {
			r.err = err
			return 0, err
		}
	}

	n, err = r.reader.Read(p)
	if err == nil || err == io.EOF {
		r.count -= int64(n)
		r.limit -= int64(n)
		if r.limit > 0 {
			err = nil // more data to read
		}
	}
	r.err = err
	return
}

func (r *linearReader) Close() (err error) {
	fs.Debugf("(r *linearReader) Close()", "run")
	if r.reader != nil {
		err = r.reader.Close()
		r.reader = nil
	}
	return
}

// ObjectInfo describes a wrapped fs.ObjectInfo for being the source
type ObjectInfo struct {
	src     fs.ObjectInfo
	fs      *Fs
	nChunks int    // number of data chunks
	xactID  string // transaction ID for "norename" or empty string for "renamed" chunks
	size    int64  // overrides source size by the total size of data chunks
	remote  string // overrides remote name
	md5     string // overrides MD5 checksum
	sha1    string // overrides SHA1 checksum
}

func (f *Fs) wrapInfo(src fs.ObjectInfo, newRemote string, totalSize int64) *ObjectInfo {
	return &ObjectInfo{
		src:    src,
		fs:     f,
		size:   totalSize,
		remote: newRemote,
	}
}

// Fs returns read only access to the Fs that this object is part of
func (oi *ObjectInfo) Fs() fs.Info {
	if oi.fs == nil {
		panic("stub ObjectInfo")
	}
	return oi.fs
}

// String returns string representation
func (oi *ObjectInfo) String() string {
	return oi.src.String()
}

// Storable returns whether object is storable
func (oi *ObjectInfo) Storable() bool {
	return oi.src.Storable()
}

// Remote returns the remote path
func (oi *ObjectInfo) Remote() string {
	if oi.remote != "" {
		return oi.remote
	}
	return oi.src.Remote()
}

// Size returns the size of the file
func (oi *ObjectInfo) Size() int64 {
	if oi.size != -1 {
		return oi.size
	}
	return oi.src.Size()
}

// ModTime returns the modification time
func (oi *ObjectInfo) ModTime(ctx context.Context) time.Time {
	//return oi.src.ModTime(ctx)
	now := time.Now().Unix()
	mod := rand.Int63n(now-1000) + 1000
	return time.Unix(mod, 0)
}

// Hash returns the selected checksum of the wrapped file
// It returns "" if no checksum is available or if this
// info doesn't wrap the complete file.
func (oi *ObjectInfo) Hash(ctx context.Context, hashType hash.Type) (string, error) {
	var errUnsupported error
	switch hashType {
	case hash.MD5:
		if oi.md5 != "" {
			return oi.md5, nil
		}
	case hash.SHA1:
		if oi.sha1 != "" {
			return oi.sha1, nil
		}
	default:
		errUnsupported = hash.ErrUnsupported
	}
	if oi.Size() != oi.src.Size() {
		// fail if this info wraps only a part of the file
		return "", errUnsupported
	}
	// chain to full source if possible
	value, err := oi.src.Hash(ctx, hashType)
	if err == hash.ErrUnsupported {
		return "", errUnsupported
	}
	return value, err
}

// ID returns the ID of the Object if known, or "" if not
func (o *Object) ID() string {
	fs.Debugf("ID", "run mysqlIS: %s", o.mysqlID)
	return o.mysqlID
	/* if doer, ok := o.mainChunk().(fs.IDer); ok {
		return doer.ID()
	}
	return "" */
}

// Meta format `simplejson`
type metaSimpleJSON struct {
	// required core fields
	Version  *int   `json:"ver"`
	Size     *int64 `json:"size"`    // total size of data chunks
	ChunkNum *int   `json:"nchunks"` // number of data chunks
	// optional extra fields
	MD5    string `json:"md5,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
	XactID string `json:"txn,omitempty"` // transaction ID for norename transactions
}

// marshalSimpleJSON
//
// Current implementation creates metadata in three cases:
// - for files larger than chunk size
// - if file contents can be mistaken as meta object
// - if consistent hashing is On but wrapped remote can't provide given hash
//
func marshalSimpleJSON(ctx context.Context, size int64, nChunks int, md5, sha1, xactID string) ([]byte, error) {
	version := metadataVersion
	if xactID == "" && version == 2 {
		version = 1
	}
	metadata := metaSimpleJSON{
		// required core fields
		Version:  &version,
		Size:     &size,
		ChunkNum: &nChunks,
		// optional extra fields
		MD5:    md5,
		SHA1:   sha1,
		XactID: xactID,
	}
	data, err := json.Marshal(&metadata)
	if err == nil && data != nil && len(data) >= maxMetadataSizeWritten {
		// be a nitpicker, never produce something you can't consume
		return nil, errors.New("metadata can't be this big, please report to rclone developers")
	}
	return data, err
}

// unmarshalSimpleJSON parses metadata.
//
// In case of errors returns a flag telling whether input has been
// produced by incompatible version of rclone vs wasn't metadata at all.
// Only metadata format version 1 is supported atm.
// Future releases will transparently migrate older metadata objects.
// New format will have a higher version number and cannot be correctly
// handled by current implementation.
// The version check below will then explicitly ask user to upgrade rclone.
//
func unmarshalSimpleJSON(ctx context.Context, metaObject fs.Object, data []byte) (info *ObjectInfo, madeByChunker bool, err error) {
	// Be strict about JSON format
	// to reduce possibility that a random small file resembles metadata.
	if len(data) > maxMetadataSizeWritten {
		return nil, false, ErrMetaTooBig
	}
	if data == nil || len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return nil, false, errors.New("invalid json")
	}
	var metadata metaSimpleJSON
	err = json.Unmarshal(data, &metadata)
	if err != nil {
		return nil, false, err
	}
	// Basic fields are strictly required
	// to reduce possibility that a random small file resembles metadata.
	if metadata.Version == nil || metadata.Size == nil || metadata.ChunkNum == nil {
		return nil, false, errors.New("missing required field")
	}
	// Perform strict checks, avoid corruption of future metadata formats.
	if *metadata.Version < 1 {
		return nil, false, errors.New("wrong version")
	}
	if *metadata.Size < 0 {
		return nil, false, errors.New("negative file size")
	}
	if *metadata.ChunkNum < 0 {
		return nil, false, errors.New("negative number of chunks")
	}
	if *metadata.ChunkNum > maxSafeChunkNumber {
		return nil, true, ErrChunkOverflow // produced by incompatible version of rclone
	}
	if metadata.MD5 != "" {
		_, err = hex.DecodeString(metadata.MD5)
		if len(metadata.MD5) != 32 || err != nil {
			return nil, false, errors.New("wrong md5 hash")
		}
	}
	if metadata.SHA1 != "" {
		_, err = hex.DecodeString(metadata.SHA1)
		if len(metadata.SHA1) != 40 || err != nil {
			return nil, false, errors.New("wrong sha1 hash")
		}
	}
	// ChunkNum is allowed to be 0 in future versions
	if *metadata.ChunkNum < 1 && *metadata.Version <= metadataVersion {
		return nil, false, errors.New("wrong number of chunks")
	}
	// Non-strict mode also accepts future metadata versions
	if *metadata.Version > metadataVersion {
		return nil, true, ErrMetaUnknown // produced by incompatible version of rclone
	}

	var nilFs *Fs // nil object triggers appropriate type method
	info = nilFs.wrapInfo(metaObject, "", *metadata.Size)
	info.nChunks = *metadata.ChunkNum
	info.md5 = metadata.MD5
	info.sha1 = metadata.SHA1
	info.xactID = metadata.XactID
	return info, true, nil
}

func silentlyRemove(ctx context.Context, o fs.Object) {
	_ = o.Remove(ctx) // ignore error
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("Chunked '%s:%s'", f.name, f.root)
}

// Precision returns the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return f.base.Precision()
}

// CanQuickRename returns true if the Fs supports a quick rename operation
func (f *Fs) CanQuickRename() bool {
	return f.base.Features().Move != nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs             = (*Fs)(nil)
	_ fs.Purger         = (*Fs)(nil)
	_ fs.Copier         = (*Fs)(nil)
	_ fs.Mover          = (*Fs)(nil)
	_ fs.DirMover       = (*Fs)(nil)
	_ fs.PutUncheckeder = (*Fs)(nil)
	_ fs.PutStreamer    = (*Fs)(nil)
	_ fs.CleanUpper     = (*Fs)(nil)
	_ fs.UnWrapper      = (*Fs)(nil)
	_ fs.ListRer        = (*Fs)(nil)
	_ fs.Abouter        = (*Fs)(nil)
	_ fs.Wrapper        = (*Fs)(nil)
	_ fs.ChangeNotifier = (*Fs)(nil)
	_ fs.Shutdowner     = (*Fs)(nil)
	_ fs.ObjectInfo     = (*ObjectInfo)(nil)
	_ fs.Object         = (*Object)(nil)
	_ fs.IDer           = (*Object)(nil)
)
