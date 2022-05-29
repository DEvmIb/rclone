// Package privacer provides wrappers for Fs and Object which split large files in chunks
//TODO: delayed deletion. on file or dir remove only metadata delete. remove files later by command. bonbon .trash folder
//			-- move to trash -- done
//			-- manage trash
//				-- auto background -- done
//				-- clean manual cmd
//TODO: folders and files additional random id needed for delayed delete. -- done
//TODO: SetModTime -- done
//TODO: move -- done
//TODO: rename -- done
//TODO: remove -- done checking
//TODO: rmdir -- done checking
//TODO: copy
//TODO: remove file to trash when overwrite -- done
//TODO: defer external sql function how to -- no solution without more code.. aborted
//TODO: honor max chunk size
//TODO: copy file to mount. will it instant added to mysql or on write-back-delay? .. on write-back..nice
//TODO: optimize mysql querys
//TODO: add failed uploads to trash or delete direct
//TODO: all switches
//TODO: sql reconnect


// copy or move is for file

package privacer

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
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
	_ "github.com/mattn/go-sqlite3"
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
			Help: `Remote to used for chunks.

Normally should contain a ':' and a path, e.g. "myremote:path/to/dir",
"myremote:bucket" or maybe "myremote:" (not recommended).`,
		}, {
			Name:     "meta_driver",
			Required: true,
			Help: `driver for metadata
			
			-- sqlite3
			-- mysql`,
		}, {
			Name:     "meta_url",
			Required: true,
			Help: `connect string for metadata
			
			-- file:/rclone-meta.db?mode=rwc&_journal_mode=wal&_txlock=immediate
			-- rclone:rclone@tcp(127.0.0.1:3306)/rclone-meta`,
		},{
			Name:     "chunk_size",
			Advanced: false,
			Default:  fs.SizeSuffix(20971520), // 10Mb
			Help:     `Files larger than chunk size will be split in chunks.`,
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
		},},
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

	if opt.MetaDriver == "" {
		return nil, fmt.Errorf("meta_driver required")
	}

	if opt.MetaUrl == "" {
		return nil, fmt.Errorf("meta_url required")
	}

	if int64(opt.ChunkSize) < 0 {
		return nil, fmt.Errorf("chunk_size required")
	}
	/* if opt.StartFrom < 0 {
		return nil, errors.New("start_from must be non-negative")
	} */

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

	var db *sql.DB
	// "file:/lxc/_db/rclone-meta-test.db?mode=rwc&_journal_mode=wal&_txlock=immediate"
	// "rclone:rclone@tcp(10.21.200.152:3306)/rclone"
	db, err = sql.Open(opt.MetaDriver, opt.MetaUrl)
	db.SetMaxOpenConns(10)
	if err != nil {
		panic("sql error")
	}

	switch opt.MetaDriver {
		case "sqlite3":
			_, _ = db.Exec(`CREATE TABLE 'meta' (
				'id' bigint unique not null default 0,
				'parent' bigint not null default 0,
				'type' int not null default 0,
				'name' text not null default '',
				'modtime' bigint  not null default 0,
				'md5' text not null default '',
				'sha1' text not null default '',
				'size' bigint  not null default 0,
				'chunks' int  not null default 0,
				'cname' text unique not null default ''
			);
			CREATE TABLE 'trash' (
				'id' bigint unique not null default 0,
				'type' int not null default 0,
				'name' text not null default '',
				'modtime' bigint  not null default 0,
				'md5' text not null default '',
				'sha1' text not null default '',
				'size' bigint  not null default 0,
				'chunks' int  not null default 0,
				'cname' text unique not null default '',
				'trashtime' bigint  not null default 0);`)
			
		  case "mysql":
			_, _ = db.Exec(`CREATE TABLE 'meta' (
				'id' bigint NOT NULL,
				'parent' bigint NOT NULL,
				'type' int NOT NULL,1
				'name' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
				'modtime' bigint NOT NULL,
				'md5' text COLLATE utf8mb4_unicode_ci NOT NULL,
				'sha1' text COLLATE utf8mb4_unicode_ci NOT NULL,
				'size' bigint NOT NULL,
				'chunks' int NOT NULL,
				'cname' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL
			  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
			  	 
			  CREATE TABLE 'trash' (
				'id' bigint NOT NULL,
				'type' int NOT NULL,
				'name' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
				'modtime' bigint NOT NULL,
				'md5' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
				'sha1' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
				'size' bigint NOT NULL,
				'chunks' int NOT NULL,
				'cname' text CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
				'trashtime' bigint DEFAULT NULL
			  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
			  
				ALTER TABLE 'meta'
				ADD PRIMARY KEY ('id','parent') USING BTREE,
				ADD UNIQUE KEY 'id' ('id','parent');
			
				ALTER TABLE 'trash'
				ADD PRIMARY KEY ('id'),
				ADD UNIQUE KEY 'id' ('id');
				COMMIT; `)
			  default:
				return nil, fmt.Errorf("wrong meta_driver ")
	}



	f := &Fs{
		base:  baseFs,
		name:  name,
		root:  rpath,
		oroot: orpath,
		opt:   *opt,
		db: db,
	}
	cache.PinUntilFinalized(f.base, f)
	f.dirSort = true // processEntries requires that meta Objects prerun data chunks atm.

	f.setHashType(opt.HashType)

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

	//trashRunnerDelete(f, ctx)
	
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
	HashType     string        `config:"hash_type"`
	Transactions string        `config:"transactions"`
	MetaDriver   string        `config:"meta_driver"`
	MetaUrl      string        `config:"meta_url"`
}

// Fs represents a wrapped fs.Fs
type Fs struct {
	name         string
	root         string
	oroot        string
	base         fs.Fs          // remote wrapped by chunker overlay
	wrapper      fs.Fs          // wrapper is used by SetWrapper
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
	db			*sql.DB
	cronTrashRunnerDeleteLock bool
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
	//requireMetaHash := true

	switch hashType {
	case "none":
		//requireMetaHash = false
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
	/* if requireMetaHash && !f.useMeta {
		//return fmt.Errorf("hash type '%s' requires compatible meta format", hashType)
	} */
	return nil
}

func trashRunnerStats(f *Fs) {
	fs.Debugf("trashRunner", "start")
	var trashInfoLine string

	trashCount, err := f.mysqlGetTrashCount()
	if err != nil {
		return
	}
	trashSize, err := f.mysqlGetTrashSize()
	if err != nil {
		return
	}

	trashInfoLine = fmt.Sprintf("(%s) Trashed Files. (%s) Total Size.",trashCount,trashSize)


	fs.Infof("trashRunner","%s",trashInfoLine)
	
}

func trashRunnerDelete(f *Fs, ctx context.Context) {
	if f.cronTrashRunnerDeleteLock {
		return
	}
	f.cronTrashRunnerDeleteLock = true
	fs.Debugf("trashRunnerDelete", "start")
	var _1s int64
	_1s = 1000000000
	_trashTime := time.Now().UnixNano() - (_1s * 86400)

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

			fs.Infof("trashRunner","Delete: %s",_record.name)

			for i := 0; i < _record.chunks; i++ {
				_cname := f.makeSha1FromString(_record.cname + fmt.Sprint(i))[0:2]+"/"+_record.cname+"."+fmt.Sprint(i)
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

			err = f.mysqlInsert("delete from trash where id=?",_record.id)
			
			if err != nil {
				fs.Infof("trashRunnerDelete","error deleting:",_record.name)
			} else {
				fs.Infof("trashRunnerDelete","Removed %s from Trash. Freed: %s",_record.name,operations.SizeString(_record.size,true))
			}

	}
	f.cronTrashRunnerDeleteLock = false
	
}

// build an sha1 string from input string
func (f *Fs) makeSha1FromString(input string) string {
	//return input
	h := sha1.New()
	h.Write([]byte(input))
	bs := h.Sum(nil)
	return hex.EncodeToString(bs)
}

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

func (f *Fs) mysqlGetFileByPath(_path string) (obj *Object, err error) {
	_di, fi := path.Split(_path)
	_id, err := f.mysqlFindMyDirId(_di)
	if err != nil {
		return
	}
	_id, err = f.mysqlFindFileId(fi, _id)
	if err != nil {
		return
	}
	o, err := f.mysqlGetFile(_id)
	if err != nil {
		return
	}
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

func (f *Fs) mysqlFindDirId(dirName string, startId string) (id string, err error) {
	fs.Debugf("mysqlFindDirId", "%s %s", dirName, startId)
	index, err := f.mysqlQuery("select id,name from meta where type=1 and parent=?", startId)
	if err != nil {
		return
	}
	defer index.Close()
	for index.Next() {
		_id := ""
		_name := ""
		err = index.Scan(&_id, &_name)
		if err != nil {
			return
		}
		if _name == dirName {
			return _id, nil
		}
	}
	return "", nil
}

func (f *Fs) mysqlFindFileId(fiName string, startId string) (id string, err error) {
	fs.Debugf("mysqlFindFileId", "%s %s", fiName, startId)
	index, err := f.mysqlQuery("select id,name from meta where type=2 and parent=?", startId)
	if err != nil {
		return
	}
	defer index.Close()
	for index.Next() {
		_id := ""
		_name := ""
		err = index.Scan(&_id, &_name)
		if err != nil {
			return
		}
		if _name == fiName {
			return _id, nil
		}
	}
	return "", nil
}

func (f *Fs) mysqlFindMyDirId(dir string) (id string, err error) {
	id = "0"
	dir = f.prefixSlash(f.cleanFolderSlashes(dir))
	if dir != "" {
		lo := strings.Split(dir, "/")
		for i := 1; i < len(lo); i++ {
			fs.Debugf("mysqlFindMyId", "find: %s", lo[i])
			//if i == 0 {
			id, err = f.mysqlFindDirId(lo[i], id)
			if err != nil {
				return
			}
			//} else {

			//}
		}
	}
	return
}

func (f *Fs) mysqlFindMyFileId(_path string) (id string, err error) {
	di, fi := path.Split(_path)
	diId, err := f.mysqlFindMyDirId(di)
	if err != nil {
		return
	}
	fiId, err := f.mysqlFindFileId(fi, diId)
	if err != nil {
		return
	}
	return fiId, nil
}

func (f *Fs) mysqlGetTrashCount() (_count string, err error) {
	_res, err := f.mysqlQuery("select count(*) from trash")
	if err != nil {
		return 
	}
	defer _res.Close()
	if err == nil {
		_res.Next()
		var _count string
		err = _res.Scan(&_count)
		if err != nil {
			return "", err
		}
		return _count, nil
	} else {
		return "0", nil
	}
}

func (f *Fs) mysqlGetTrashSize() (res string, err error) {
	_res, err := f.mysqlQuery("select sum(size) from trash")
	if err == nil {
		defer _res.Close()
		_res.Next()
		var _count int64
		_res.Scan(&_count)
		return operations.SizeString(_count,true), nil
	} else {
		return "0B", nil
	}
}

func (f *Fs) mysqlBuildList(dir string, nroot string) (entries fs.DirEntries, err error) {
	entries = nil
	res, err := f.mysqlIsFile(nroot)
	if err != nil {
		return nil, err
	}
	if res {
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
			_id, err = f.mysqlFindDirId(lo[i], _id)
			if err != nil {
				return nil, err
			}
			//} else {

			//}
		}
	}

	fs.Debugf("mysqlBuildList", "found my id: %s", _id)
	index, err := f.mysqlQuery("select * from meta where parent=?", _id)
	if err != nil {
		return nil, err
	}
	defer index.Close()
	if err != nil {
		panic(err)
	}

	for index.Next() {
		var entry mysqlEntry
		err = index.Scan(&entry.ID, &entry.Parent, &entry.Type, &entry.Name, &entry.ModTime, &entry.Md5, &entry.Sha1, &entry.Size, &entry.Chunks, &entry.CName)
		if err != nil {
			return
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

	isdir, err := f.mysqlDirExists(nroot)
	if err != nil {
		return nil, err
	}
	isfile, err := f.mysqlIsFile(nroot)
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
	entries, err = f.mysqlBuildList(dir, nroot)
	if err != nil {
		return nil, err
	}
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
	ex, err := f.mysqlIsFile(remote)
	if err != nil {
		return nil, err
	}
	if ex {
		fs.Debugf("NewObject", " Found: %s", remote)
		_o, err := f.mysqlGetFileByPath(remote)
		if err != nil {
			return nil, err
		}
		return _o, err
	}
	fs.Debugf("NewObject", "not found: %s", remote)
	return nil, fs.ErrorObjectNotFound
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

	err = f.mysqlMkDirRe(f.oroot)
	if err != nil {
		return
	}

	c := f.newChunkingReader(src)


	wrapIn := c.wrapStream(ctx, in, src)

	minChunks := int64(2)

	//body return check must be
	var min, max int64
	if c.sizeTotal <= 1024 {
		min = -1
		max = -1
	} else {
		if c.sizeTotal <= int64(f.opt.ChunkSize) {
			//s := c.sizeTotal / 4 / 2
			//min = s
			//max = c.sizeTotal - s
			s := c.sizeTotal
			for i := s; i > 0; i-- {
				//fs.Debugf("1","%s",c.sizeTotal/i)
				if ( c.sizeTotal / i ) >= minChunks {
					s = i
					break
				}
				
			}
			//s := int64(f.opt.ChunkSize) / int64(minChunks)
			min = s - ( s / 4 ) 
			max = s
		} else if c.sizeTotal > int64(f.opt.ChunkSize) {
			s := int64(f.opt.ChunkSize)
			for i := s; i > 0; i-- {
				//fs.Debugf("1","%s",c.sizeTotal/i)
				if ( c.sizeTotal / i ) >= minChunks {
					s = i
					break
				}
				
			}
			//s := int64(f.opt.ChunkSize) / int64(minChunks)
			min = s - (s / 4)
			max = s
		}
		//c.chunkSize = max1
	}

	fs.Debugf("put","chunk params min: %s max: %s",min,max)

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

		info := &ObjectInfo{
			src:    src,
			fs:     f,
			size:   size,
			remote: f.makeSha1FromString(cname + fmt.Sprint(c.chunkNo))[0:2]+"/"+cname+"."+fmt.Sprint(c.chunkNo),
		}

		//info := f.wrapInfo(src, f.makeSha1FromString(cname + fmt.Sprint(c.chunkNo))[0:2]+"/"+cname+"."+fmt.Sprint(c.chunkNo), size)
		
		chunk, err := basePut(ctx, wrapIn, info, options...)
		c.chunks = append(c.chunks, chunk)
		
		if err != nil {
			//oerr := err
			fs.Debugf("put","error uploading removing chunks")
			for _, chunk := range c.chunks {
				fs.Debugf("put","removing: %s",chunk.Remote())
				ue := chunk.Remove(ctx)
				if ue != nil {
					fs.Debugf("put","failed to remove. chunk may be never uploaded. run fsck is adviced")
				}
			}
			return nil, err
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
	pid, err := f.mysqlFindMyDirId(di)      //f.prefixSlash(f.cleanFolderSlashes(di))
	if err != nil {
		return nil , err
	}
	//fs.Debugf("muh1","%s",f.mysqlFindParentId(di))
	//fs.Debugf("muh2","%s",f.mysqlFindMyId(di))
	fs.Debugf("putid", "%s", nid)
	res, err := f.mysqlIsFile(_dst)
	if err != nil {
		return nil, err
	}
	if res {
		fs.Debugf("put", "file exist move to trash")
		//TODO: error check
		err = f.mysqlDeleteFile(_dst)
		if err != nil {
			return
		}
	}
	modT := src.ModTime(ctx).UnixNano()
	//TODO: error handling
	err = f.mysqlInsert("insert into meta (id,parent,type,name,modtime,md5,sha1,size,chunks,cname) values (?,?,?,?,?,?,?,?,?,?)", nid, pid, 2, fi, modT, o.md5, o.sha1, c.sizeTotal, c.chunkNo, cname)
	if err != nil {
		//oerr := err
		fs.Debugf("put","error inserting metadata into database. removing chunks")
			for _, chunk := range c.chunks {
				fs.Debugf("put","removing: %s",chunk.Remote())
				ue := chunk.Remove(ctx)
				if ue != nil {
					fs.Debugf("put","failed to remove. chunk may be never uploaded. run fsck is adviced")
				}
			}
		return nil, fmt.Errorf("error inserting metadata into database %s",err)
	}
	fs.Debugf("put","%s",err)
	//defer _r.Close()
	//TODO: err handle
	if err != nil {
		panic(err)
	}
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
	/* if c.chunkNo == 0 && c.expectSingle && bytesRead > 0 && c.readCount <= maxMetadataSize {
		c.smallHead = append(c.smallHead, buf[:bytesRead]...)
	} */
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
	/* if c.hasher == nil && c.readCount+size > maxMetadataSize {
		c.accountBytes(size)
		return nil
	} */
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

func (f *Fs) mysqlInsert(_query string, args ...interface{}) (err error) {
	fs.Debugf("mysqlInsert", "Query: %s a: %s", _query, args)
/* 	use_sqlite := true
	
	var db *sql.DB
	if use_sqlite {
		//   /lxc/_db/rclone-meta-test.db
		db, err = sql.Open("sqlite3", "file:/lxc/_db/rclone-meta-test.db?cache=shared&mode=rwc&_journal=WAL&_busy_timeout=5000")
		db.SetMaxOpenConns(1)
	} else {
		db, err = sql.Open("mysql", "rclone:rclone@tcp(10.21.200.152:3306)/rclone")
	}
	defer db.Close()
	if err != nil {
		return
	} */
	fs.Debugf("mysqlInsert","Prepare")
	p, err := f.db.Prepare(_query)
	if err != nil {
		fs.Debugf("mysqlInsert","err: %s",err)
		return
	}
	fs.Debugf("mysqlInsert","Prepare end")
	defer p.Close()
	fs.Debugf("mysqlInsert","run query")
	_, err = p.Exec(args...)
	if err != nil {
		fs.Debugf("mysqlInsert","err: %s",err)
		return
	}
	return
}

func (f *Fs) mysqlQuery(_query string, args ...interface{}) (res *sql.Rows, err error) {
	fs.Debugf("mysqlQuery", "Query: %s a: %s", _query, args)
	/* 	use_sqlite := true
	
	var db *sql.DB
	if use_sqlite {
		//   /lxc/_db/rclone-meta-test.db
		db, err = sql.Open("sqlite3", "file:/lxc/_db/rclone-meta-test.db?cache=shared&mode=rwc&_journal=WAL&_busy_timeout=5000")
		db.SetMaxOpenConns(1)
	} else {
		db, err = sql.Open("mysql", "rclone:rclone@tcp(10.21.200.152:3306)/rclone")
	}
	if err != nil {
		panic(err.Error())
	}
	defer db.Close()
 */
	res, err = f.db.Query(_query, args...)
	if err != nil {
		return nil, err
	}
	//defer res.Close()
	return
}

func (f *Fs) mysqlDirExists(_path string) (res bool, err error) {
	// root
	if _path == "" || _path == "/" {
		return true, nil
	}

	_r, err := f.mysqlFindMyDirId(_path)
	if err != nil {
		return
	}
	if _r == "" {
		return false, nil
	} else {
		return true, nil
	}
}

func (f *Fs) mysqlChangeDirParent(idFrom string, idTo string) (err error) {
	err = f.mysqlInsert("update meta set id=? where id=?", idFrom, idTo)
	if err != nil {
		return
	}
	//defer index.Close()
	return
}

func (f *Fs) mysqlDirIsEmpty(id string) (res bool,err error) {
	// root
	/* if path == "" {
		return false
	} */
	index, err := f.mysqlQuery("select id from meta where parent=? limit 1", id)
	if err != nil {
		return
	}
	defer index.Close()
	state := index.Next()
	if state == false {
		fs.Debugf("mysqlDirIsEmpty", "true")
		return true, nil
	} else {
		fs.Debugf("mysqlDirIsEmpty", "false")
		return false, nil
	}
}

func (f *Fs) mysqlIsFile(_path string) (res bool, err error) {
	if _path == "" {
		return false, nil
	}
	di, fi := path.Split(_path)
	pid, err := f.mysqlFindMyDirId(di)
	if err != nil {
		return
	}
	fs.Debugf("mysqlIsFile", "pid: %s", pid)
	index, err := f.mysqlQuery("select * from meta where type=2 and name=? and parent=?", fi, pid)
	if err != nil {
		return
	}
	defer index.Close()
	//index.Next()
	//count, _ := index.Columns()
	state := index.Next()
	fs.Debugf("mysqlIsFile", "state: %s", state)
	return state, nil
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
	err = f.mysqlInsert("update meta set modtime=? where id=?", mod, id)
	//defer index.Close()
	return

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

func (f *Fs) mysqlMkDirRe(_path string) error {

	_path = f.cleanFolderSlashes(_path)
	fs.Debugf("mysqlMkDirRe", "start: %s", _path)
	root := strings.Split(_path, "/")
	if _path == "" {
		return nil
	}
	//root = append([]string{""},root...)
	//dname, _ := path.Split(_path)
	_p := ""
	_pid := "0"
	for _, name := range root {
		_pnew, err := f.mysqlFindDirId(name, _pid)
		if err != nil {
			return err
		}
		fs.Debugf("1. mysqlMkDirRe", "_pnew: %s _pid: %s name: %s", _pnew, _pid, name)
		_nid := ""
		if _pnew == "" {
			_nid = fmt.Sprint(f.getRandomId())
			fs.Debugf("2. mysqlMkDirRe in", "_pnew: %s _pid: %s name: %s", _pnew, _pid, name)
			//fs.Debugf("test","%s",time.Now().UnixMilli())
			//_r, _ := f.mysqlQuery("insert ignore into meta (id, parent, type, name, modtime) values(?, ?, ?, ? ,?)",f.makeSha1FromString(_p+"/"+name), f.makeSha1FromString(_p), 1, name, time.Now().UnixNano())
			//TODO error handling
			err = f.mysqlInsert("insert into meta (id, parent, type, name, modtime, cname) values(?, ?, ?, ? ,?, ?)", _nid, _pid, 1, name, time.Now().UnixNano(),_nid)
			if err != nil {
				return err
			}
			//defer _r.Close()
		}
		_p += "/" + name

		_pid, err = f.mysqlFindMyDirId(_p)
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {

	root := path.Join(f.oroot, dir)
	//_, dname := path.Split(root)

	fs.Debugf("MKdir", "go dir: %s f.oroot: %s root: %s", dir, f.oroot, root)

	if root == "" {
		// no folder, silence discard
		return nil
	}

	err := f.mysqlMkDirRe(root)

	return err
}

// Rmdir removes the directory (container, bucket) if empty
//
// Return an error if it doesn't exist or isn't empty

func (f *Fs) mysqlRmDir(path string) (err error) {

	return nil
}

func (f *Fs) mysqlRmDirByID(id string) (err error) {
	err = f.mysqlInsert("delete from meta where type=1 and id=?", id)
	return
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Debugf("RmDir", "run %s oroot: %s", dir, f.oroot)
	_id, err := f.mysqlFindMyDirId(dir)
	if err != nil {
		return err
	}
	res, err := f.mysqlDirIsEmpty(_id)
	if err != nil {
		return err
	}
	if res {
		err = f.mysqlRmDirByID(_id)
		if err != nil {
			return err
		}
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

	_did, err := f.mysqlFindMyDirId(di)

	if err != nil {
		return
	}

	_fid, err := f.mysqlFindFileId(fi, _did)
	if err != nil {
		return
	}
	o, err := f.mysqlGetFile(_fid)
	if err != nil {
		return
	}

	err = f.mysqlInsert("delete from meta where id=?", _fid)
	if err != nil {
		return
	}
	//defer _r.Close()

	o.mysqlID = fmt.Sprint(f.getRandomId())

	//TODO: error handling
	err = f.mysqlInsert("insert into trash (id,type,name,modtime,md5,sha1,size,chunks,cname,trashtime) values(?,?,?,?,?,?,?,?,?,?)", o.mysqlID, 2, o.remote, o.modTime.UnixNano(), o.md5, o.sha1, o.size, o.chunksC, o.cName, o.mysqlID)
	if err != nil {
		return
	}
	//defer _r.Close()
	return

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
	res, err := f.mysqlIsFile(remote)
	if err != nil {
		return nil, err
	}
	if res {
		return nil, fs.ErrorDirExists
	}
	di, _ := path.Split(remote)
	_dstDiEx, err := f.mysqlDirExists(di)
	if err != nil {
		return nil, err
	}
	if !_dstDiEx {
		return nil, fs.ErrorDirNotFound
	}

	_dstDiId, err := f.mysqlFindMyDirId(di)
	if err != nil {
		return nil, err
	}
	_dstFiId, err := f.mysqlFindMyFileId(src.Remote())
	if err != nil {
		return nil, err
	}
	fs.Debugf("Move", "_dstDiId: %s _dstFiId: %s", _dstDiId, _dstFiId)

	err = f.mysqlMoveFile(_dstFiId, _dstDiId)
	if err != nil {
		return nil, err
	}

	o, err := f.mysqlGetFileByPath(remote)

	return o, err
}

func (f *Fs) mysqlMoveDir(id string, dstDirId string) (err error) {
	err = f.mysqlInsert("update meta set parent=? where id=?", dstDirId, id)
	fs.Debugf("mysqlMoveDir", "err", err)
	return

}

func (f *Fs) mysqlMoveFile(id string, dstDirId string) (err error) {
	err = f.mysqlInsert("update meta set parent=? where id=?", dstDirId, id)
	fs.Debugf("mysqlMoveDir", "err", err)
	return

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

	_idSrc, err := f.mysqlFindMyDirId(srcRemote)
	if err != nil {
		return err
	}
	_idDst, err := f.mysqlFindMyDirId(dstRemote)
	if err != nil {
		return err
	}

	_di, _ := path.Split(dstRemote)

	_idDstP, err := f.mysqlFindMyDirId(_di)
	if err != nil {
		return err
	}

	fs.Debugf("DirMove", "idSrc: %s idDst: %s idDstP: %s", _idSrc, _idDst, _idDstP)

	// better than moving
	if _idDst != "" {
		return fs.ErrorDirExists
	}
	if _idDstP == "" {
		return fs.ErrorDirNotFound
	}

	//f.mysqlMkDirRe(dstRemote)

	_idDst, err = f.mysqlFindMyDirId(dstRemote)
	if err != nil {
		return err
	}

	err = f.mysqlMoveDir(_idSrc, _idDstP)
	if err != nil {
		return err
	}
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
	do := f.base.Features().ChangeNotify
	if do == nil {
		return
	}
	wrappedNotifyFunc := func(path string, entryType fs.EntryType) {
		fs.Debugf("ChangeNotify", "sub %s",path)
		//fs.Debugf(f, "ChangeNotify: path %q entryType %d", path, entryType)
		/* if entryType == fs.EntryObject {
			mainPath, _, _, xactID := f.parseChunkName(path)
			metaXactID := ""
			if f.useNoRename {
				metaObject, _ := f.base.NewObject(ctx, mainPath)
				dummyObject := f.newObject("", metaObject, nil)
				metaXactID, _ = dummyObject.readXactID(ctx)
			}
			if mainPath != "" && xactID == metaXactID {
				path = mainPath
			}
		} */
		notifyFunc(path, entryType)
	}
	do(ctx, wrappedNotifyFunc, pollIntervalChan)
	


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
			cn := o.f.makeSha1FromString(o.cName + fmt.Sprint(i))[0:2] + "/" + o.cName + "." + fmt.Sprint(i)
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
