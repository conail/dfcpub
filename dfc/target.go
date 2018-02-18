// Package dfc provides distributed file-based cache with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2017, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OneOfOne/xxhash"
	"github.com/golang/glog"
)

// xattrs
const (
	ObjstateAttr = "user.obj.isvalid" // Object's state
	HashAttr     = "user.obj.dfchash" // Object's XXHash
	XAttrInvalid = "0"
	XAttrValid   = "1"
)

const DFC_CONTENT_HASH = "Content-HASH"

type mountPath struct {
	Path string
	Fsid syscall.Fsid
}

type fipair struct {
	relname string
	os.FileInfo
}
type allfinfos struct {
	finfos     []fipair
	rootLength int
}

//===========================================================================
//
// target runner
//
//===========================================================================
type targetrunner struct {
	httprunner
	cloudif   cloudif // multi-cloud vendor support
	smap      *Smap
	xactinp   *xactInProgress
	starttime time.Time
	lbmap     *lbmap
	rtnamemap *rtnamemap
	rand      *rand.Rand
}

// start target runner
func (t *targetrunner) run() error {
	// init
	t.httprunner.init(getstorstats())
	t.smap = &Smap{}                                 // cluster map
	t.xactinp = newxactinp()                         // extended actions
	t.lbmap = &lbmap{LBmap: make(map[string]string)} // local (cache-only) buckets
	t.rtnamemap = newrtnamemap(128)                  // lock/unlock name

	if err := t.register(); err != nil {
		glog.Errorf("Target %s failed to register with proxy, err: %v", t.si.DaemonID, err)
		if match, _ := regexp.MatchString("connection refused", err.Error()); match {
			glog.Errorf("Target %s: retrying registration ...", t.si.DaemonID)
			time.Sleep(time.Second * 3)
			if err = t.register(); err != nil {
				glog.Errorf("Target %s failed to register with proxy, err: %v", t.si.DaemonID, err)
				glog.Errorf("Target %s is terminating", t.si.DaemonID)
				return err
			}
			glog.Errorf("Success: target %s registered OK", t.si.DaemonID)
		} else {
			return err
		}
	}
	// fill-in mpaths
	ctx.mountpaths = make(map[string]*mountPath, len(ctx.config.FSpaths))
	if t.testingFSPpaths() {
		glog.Infof("Warning: configuring %d fspaths for testing", ctx.config.TestFSP.Count)
		t.testCachepathMounts()
	} else {
		t.fspath2mpath()
		t.mpath2Fsid() // enforce FS uniqueness
	}
	for mpath := range ctx.mountpaths {
		cloudbctsfqn := mpath + "/" + ctx.config.CloudBuckets
		if err := CreateDir(cloudbctsfqn); err != nil {
			glog.Fatalf("FATAL: cannot create cloud buckets dir %q, err: %v", cloudbctsfqn, err)
		}
		localbctsfqn := mpath + "/" + ctx.config.LocalBuckets
		if err := CreateDir(localbctsfqn); err != nil {
			glog.Fatalf("FATAL: cannot create local buckets dir %q, err: %v", localbctsfqn, err)
		}
	}

	// cloud provider
	if ctx.config.CloudProvider == amazoncloud {
		// TODO: sessions
		t.cloudif = &awsif{}

	} else {
		assert(ctx.config.CloudProvider == googlecloud)
		t.cloudif = &gcpif{}
	}
	if ctx.config.NoXattrs {
		glog.Infof("Warning: running with xattrs disabled")
		glog.Flush()
	}
	// init capacity
	rr := getstorstatsrunner()
	rr.initCapacity()
	//
	// REST API: register storage target's handler(s) and start listening
	//
	t.httprunner.registerhdlr("/"+Rversion+"/"+Rfiles+"/", t.filehdlr)
	t.httprunner.registerhdlr("/"+Rversion+"/"+Rdaemon, t.daemonhdlr)
	t.httprunner.registerhdlr("/"+Rversion+"/"+Rdaemon+"/", t.daemonhdlr) // FIXME
	t.httprunner.registerhdlr("/", invalhdlr)

	// Register pprof handlers
	t.httprunner.registerhdlr("/debug/pprof/", pprof.Index)
	t.httprunner.registerhdlr("/debug/pprof/cmdline", pprof.Cmdline)
	t.httprunner.registerhdlr("/debug/pprof/profile", pprof.Profile)
	t.httprunner.registerhdlr("/debug/pprof/symbol", pprof.Symbol)
	t.httprunner.registerhdlr("/debug/pprof/trace", pprof.Trace)

	glog.Infof("Target %s is ready", t.si.DaemonID)
	glog.Flush()
	t.starttime = time.Now()
	src := rand.NewSource(int64(xxhash.ChecksumString64S(t.si.DaemonID, mLCG32)))
	t.rand = rand.New(src)
	return t.httprunner.run()
}

// stop gracefully
func (t *targetrunner) stop(err error) {
	glog.Infof("Stopping %s, err: %v", t.name, err)
	sleep := t.xactinp.abortAll()
	close(t.rtnamemap.abrt)
	t.unregister()
	t.httprunner.stop(err)
	if sleep {
		time.Sleep(time.Second)
	}
}

// target registration with proxy
func (t *targetrunner) register() error {
	jsbytes, err := json.Marshal(t.si)
	if err != nil {
		glog.Errorf("Unexpected failure to json-marshal %+v, err: %v", t.si, err)
		return err
	}
	url := ctx.config.Proxy.URL + "/" + Rversion + "/" + Rcluster
	_, err = t.call(url, http.MethodPost, jsbytes)
	return err
}

func (t *targetrunner) unregister() error {
	url := ctx.config.Proxy.URL + "/" + Rversion + "/" + Rcluster
	url += "/" + Rdaemon + "/" + t.si.DaemonID
	_, err := t.call(url, http.MethodDelete, nil)
	return err
}

//==============
//
// http handlers
//
//==============

// "/"+Rversion+"/"+Rfiles
func (t *targetrunner) filehdlr(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		t.httpfilget(w, r)
	case http.MethodPut:
		t.httpfilput(w, r)
	case http.MethodDelete:
		t.httpfildelete(w, r)
	default:
		invalhdlr(w, r)
	}
}

// "/"+Rversion+"/"+Rfiles+"/"+bucket [+"/"+objname]
//
// checks if the object exists locally (if not, downloads it)
// and sends it back via http
func (t *targetrunner) httpfilget(w http.ResponseWriter, r *http.Request) {
	var md5hash string
	apitems := t.restAPIItems(r.URL.Path, 5)
	if apitems = t.checkRestAPI(w, r, apitems, 1, Rversion, Rfiles); apitems == nil {
		return
	}
	bucket, objname, errstr := apitems[0], "", ""
	if len(apitems) > 1 {
		objname = apitems[1]
	}
	if strings.Contains(bucket, "/") {
		errstr = fmt.Sprintf("Invalid bucket name %s (contains '/')", bucket)
		t.invalmsghdlr(w, r, errstr)
		return
	}
	//
	// list the bucket and return
	//
	if len(objname) == 0 {
		t.listbucket(w, r, bucket)
		return
	}

	//
	// serialize on the name
	//
	fqn := t.fqn(bucket, objname)
	uname := bucket + objname
	exclusive := true
	t.rtnamemap.lockname(uname, exclusive, &pendinginfo{Time: time.Now(), fqn: fqn}, time.Second)
	defer func(locktype *bool) {
		t.rtnamemap.unlockname(uname, *locktype)
	}(&exclusive)

	//
	// get the object from the bucket
	//
	coldget := false
	_, err := os.Stat(fqn)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			if t.islocalBucket(bucket) {
				errstr := fmt.Sprintf("GET local: file %s (local bucket %s, object %s) does not exist",
					fqn, bucket, objname)
				t.invalmsghdlr(w, r, errstr, http.StatusInternalServerError)
				return
			}
			coldget = true
		case os.IsPermission(err):
			errstr = fmt.Sprintf("Permission denied: access forbidden to %s", fqn)
			t.invalmsghdlr(w, r, errstr, http.StatusForbidden)
			return
		default:
			errstr = fmt.Sprintf("Failed to fstat %s, err: %v", fqn, err)
			t.invalmsghdlr(w, r, errstr, http.StatusInternalServerError)
			return
		}
	}

	if !coldget && isinvalidobj(fqn) {
		if t.islocalBucket(bucket) {
			errstr := fmt.Sprintf("GET local: bad checksum, file %s, local bucket %s", fqn, bucket)
			t.invalmsghdlr(w, r, errstr, http.StatusInternalServerError)
			return
		}
		glog.Warningf("Warning: %s has an invalid checksum and will be overridden", fqn)
		coldget = true
	}
	if coldget {
		if md5hash, errstr = getcloudif().getobj(fqn, bucket, objname); errstr != "" {
			t.invalmsghdlr(w, r, errstr, http.StatusInternalServerError)
			return
		}
		t.statsif.add("numcoldget", 1)
	}
	//
	// downgrade lock(name)
	//
	exclusive = false
	t.rtnamemap.downgradelock(uname)

	//
	// sendfile
	//
	file, err := os.Open(fqn)
	if err != nil {
		if os.IsPermission(err) {
			errstr = fmt.Sprintf("Permission denied: access forbidden to %s", fqn)
			t.invalmsghdlr(w, r, errstr, http.StatusForbidden)
		} else {
			errstr = fmt.Sprintf("Failed to open local file %s, err: %v", fqn, err)
			t.invalmsghdlr(w, r, errstr, http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()
	if !coldget {
		md5binary, errstr := Getxattr(fqn, HashAttr)
		if errstr != "" {
			t.invalmsghdlr(w, r, errstr)
			return
		}
		md5hash = string(md5binary)
	}
	if md5hash != "" {
		w.Header().Add(DFC_CONTENT_HASH, md5hash)
	}
	written, err := copyBuffer(w, file)
	if err != nil {
		errstr = fmt.Sprintf("Failed to send file %s, err: %v", fqn, err)
		t.invalmsghdlr(w, r, errstr)
	} else if glog.V(3) {
		glog.Infof("GET: sent %s (%.2f MB)", fqn, float64(written)/1000/1000)
	}
	t.statsif.add("numget", 1)
	glog.Flush()
}

func (t *targetrunner) listbucket(w http.ResponseWriter, r *http.Request, bucket string) {
	msg := &GetMsg{}
	if t.readJSON(w, r, msg) != nil {
		return
	}
	if !t.islocalBucket(bucket) {
		errstr := getcloudif().listbucket(w, bucket, msg)
		if errstr != "" {
			t.invalmsghdlr(w, r, errstr)
		} else {
			t.statsif.add("numlist", 1)
		}
		return
	}
	// local bucket
	allfinfos := allfinfos{make([]fipair, 0, 128), 0}
	for mpath := range ctx.mountpaths {
		localbucketfqn := mpath + "/" + ctx.config.LocalBuckets + "/" + bucket
		allfinfos.rootLength = len(localbucketfqn)
		if err := filepath.Walk(localbucketfqn, allfinfos.listwalkf); err != nil {
			glog.Errorf("Failed to traverse mpath %q, err: %v", mpath, err)
		}
	}
	t.statsif.add("numlist", 1)
	var reslist = BucketList{Entries: make([]*BucketEntry, 0, len(allfinfos.finfos))}
	for _, fi := range allfinfos.finfos {
		entry := &BucketEntry{}
		entry.Name = fi.relname
		if strings.Contains(msg.GetProps, GetPropsSize) {
			entry.Size = fi.Size()
		}
		if strings.Contains(msg.GetProps, GetPropsCtime) {
			t := fi.ModTime()
			switch msg.GetTimeFormat {
			case "":
				fallthrough
			case RFC822:
				entry.Ctime = t.Format(time.RFC822)
			default:
				entry.Ctime = t.Format(msg.GetTimeFormat)
			}
		}
		if strings.Contains(msg.GetProps, GetPropsChecksum) {
			fqn := t.fqn(bucket, fi.relname)
			md5hex, errstr := Getxattr(fqn, HashAttr)
			if errstr != "" {
				glog.Infoln(errstr)
			} else {
				md5 := hex.EncodeToString(md5hex)
				entry.Checksum = md5
			}
		}
		// TODO: other GetMsg props TBD
		reslist.Entries = append(reslist.Entries, entry)
	}
	jsbytes, err := json.Marshal(reslist)
	assert(err == nil, err)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsbytes)
}

func (all *allfinfos) listwalkf(fqn string, osfi os.FileInfo, err error) error {
	if err != nil {
		glog.Errorf("listwalkf callback invoked with err: %v", err)
		return err
	}
	if osfi.IsDir() {
		// Listbucket doesn't need to return directories
		return nil
	}
	relname := fqn[all.rootLength:]
	all.finfos = append(all.finfos, fipair{relname, osfi})
	return nil
}

func (t *targetrunner) httpfilput(w http.ResponseWriter, r *http.Request) {
	apitems := t.restAPIItems(r.URL.Path, 5)
	if len(apitems) > 2 && apitems[0] == Rfrom || apitems[2] == Rto {
		// case: rebalance "/"+Rversion+"/"+Rfiles+"/"+"from_id"+"/"+ID+"to_id"+"/"+bucket+"/"+objname
		apitems := t.restAPIItems(r.URL.Path, 6)
		if apitems := t.checkRestAPI(w, r, apitems, 6, Rversion, Rfiles); apitems == nil {
			return
		}
		from, to, bucket, objname := apitems[1], apitems[3], apitems[4], apitems[5]
		errstr := t.dorebalance(r, from, to, bucket, objname)
		if errstr != "" {
			t.invalmsghdlr(w, r, errstr)
			return
		}
		t.statsif.add("numrecvfile", 1)
	} else {
		// case: PUT
		if apitems = t.checkRestAPI(w, r, apitems, 1, Rversion, Rfiles); apitems == nil {
			return
		}
		bucket, objname := apitems[0], ""
		if len(apitems) > 1 {
			objname = strings.Join(apitems[1:], "/")
		}
		errstr := t.doput(w, r, bucket, objname)
		if errstr != "" {
			t.invalmsghdlr(w, r, errstr)
			return
		}
		t.statsif.add("numput", 1)
	}
	return
}

func (t *targetrunner) doput(w http.ResponseWriter, r *http.Request, bucket string, objname string) (errstr string) {
	var (
		err     error
		file    *os.File
		md5hash string
		hdhash  string
	)
	fqn := t.fqn(bucket, objname)
	putfqn := fmt.Sprintf("%s.%d.%d", fqn, time.Now().UnixNano(), t.rand.Intn(100))
	hdhash = r.Header.Get("Content-HASH")
	if hdhash == "" {
		glog.Infof("Warning: empty Content-HASH")
	}

	defer r.Body.Close()
	glog.Infof("PUT: %s => %s", fqn, putfqn)

	// optimize the new PUT out if the checksums match
	if hdhash != "" && !isinvalidobj(fqn) {
		file, err := os.Open(fqn)
		if err == nil {
			md5 := md5.New()
			md5hash, _ = CalculateMD5(file, md5)
			// not a critical error
			if err = file.Close(); err != nil {
				glog.Warningf("Unexpected failure to close %s once md5-ed, err: %v", fqn, err)
			}
			if md5hash == hdhash {
				glog.Infof("Existing %s is valid: new PUT is a no-op", fqn)
				return
			}
		} else {
			glog.Warningf("Unexpected failure to open local valid %s, err: %v", fqn, err)
		}
	}
	if _, md5hash, errstr = ReceiveFileAndFinalize(putfqn, objname, hdhash, r.Body); errstr != "" {
		return
	}
	// putfqn is received
	defer func() {
		if errstr != "" {
			if err = os.Remove(putfqn); err != nil {
				glog.Errorf("Nested error %s => (remove %s => err: %v)", errstr, putfqn, err)
			}
		}
	}()

	if hdhash != "" && md5hash != hdhash {
		errstr = fmt.Sprintf("Invalid checksum: %s md5 %s... does not match user's %s...", fqn, md5hash[:8], hdhash[:8])
		return
	}
	if !t.islocalBucket(bucket) {
		if file, err = os.Open(putfqn); err != nil {
			errstr = fmt.Sprintf("Failed to re-open %s err: %v", putfqn, err)
			return
		}
		if errstr = getcloudif().putobj(file, bucket, objname); errstr != "" {
			file.Close()
			return
		}
		if err = file.Close(); err != nil {
			glog.Errorf("Unexpected failure to close an already PUT file %s, err: %v", putfqn, err)
		}
	}

	// serialize on the name - and rename
	uname := bucket + objname
	t.rtnamemap.lockname(uname, true, &pendinginfo{Time: time.Now(), fqn: fqn}, time.Second)
	if err = os.Rename(putfqn, fqn); err != nil {
		errstr = fmt.Sprintf("Unexpected failure to rename %s => %s, err: %v", putfqn, fqn, err)
	} else {
		glog.Infof("PUT done: %s <= %s", fqn, putfqn)
		t.statsif.add("numput", 1)
	}
	t.rtnamemap.unlockname(uname, true)
	return
}

func (t *targetrunner) dorebalance(r *http.Request, from, to, bucket, objname string) (errstr string) {
	if glog.V(3) {
		glog.Infof("File copy: from %q bucket %q objname %q => to %q", from, bucket, objname, to)
	}
	if t.si.DaemonID != from && t.si.DaemonID != to {
		errstr = fmt.Sprintf("File copy: %s is not the intended source %s nor the destination %s",
			t.si.DaemonID, from, to)
		return
	}
	fqn := t.fqn(bucket, objname)
	finfo, err := os.Stat(fqn)

	// FIXME: TODO: serialize on the name

	if t.si.DaemonID == from {
		//
		// the source
		//
		if err != nil && os.IsNotExist(err) {
			errstr = fmt.Sprintf("File copy: %s does not exist at the source %s", fqn, t.si.DaemonID)
			return
		}
		si, ok := t.smap.Smap[to]
		if !ok {
			errstr = fmt.Sprintf("File copy: unknown destination %s (do syncsmap?)", to)
			return
		}
		if errstr = t.sendfile(r.Method, bucket, objname, si); errstr != "" {
			return
		}
		if glog.V(3) {
			glog.Infof("Sent %q to %s (size %.2f MB)", fqn, to, float64(finfo.Size())/1000/1000)
		}
	} else {
		//
		// the destination
		//
		if err != nil && os.IsExist(err) && !isinvalidobj(fqn) {
			glog.Infof("File copy: %s already exists at the destination %s", fqn, t.si.DaemonID)
			return // not an error, nothing to do
		}
		// write file with extended attributes .
		if _, _, errstr = ReceiveFileAndFinalize(fqn, objname, "", r.Body); errstr != "" {
			return
		}
	}
	return
}

func (t *targetrunner) httpfildelete(w http.ResponseWriter, r *http.Request) {
	var bucket, objname string
	apitems := t.restAPIItems(r.URL.Path, 5)
	if apitems = t.checkRestAPI(w, r, apitems, 1, Rversion, Rfiles); apitems == nil {
		return
	}
	bucket, objname = apitems[0], ""
	if len(apitems) > 1 {
		objname = strings.Join(apitems[1:], "/")
	}

	fqn := t.fqn(bucket, objname)
	uname := bucket + objname
	t.rtnamemap.lockname(uname, true, &pendinginfo{Time: time.Now(), fqn: fqn}, time.Second)
	defer t.rtnamemap.unlockname(uname, true)

	var errstr string
	if !t.islocalBucket(bucket) {
		errstr = getcloudif().deleteobj(bucket, objname)
	}
	if errstr != "" {
		t.invalmsghdlr(w, r, errstr)
		return
	}
	_, err := os.Stat(fqn)
	if err != nil {
		// Non existent object is error for LB.
		if os.IsNotExist(err) && t.islocalBucket(bucket) {
			errstr = fmt.Sprintf("DELETE local: file %s (local bucket %s, object %s) does not exist",
				fqn, bucket, objname)
		}
	} else {

		if err := os.Remove(fqn); err != nil {
			errstr = err.Error()
		}
	}
	if errstr != "" {
		t.invalmsghdlr(w, r, errstr)
		return
	}
	t.statsif.add("numdelete", 1)
}

func (t *targetrunner) sendfile(method, bucket, objname string, destsi *daemonInfo) string {
	var (
		md5hash, errstr string
	)
	fromid, toid := t.si.DaemonID, destsi.DaemonID // source=self and destination
	fqn := t.fqn(bucket, objname)
	url := destsi.DirectURL + "/" + Rversion + "/" + Rfiles + "/"
	url += Rfrom + "/" + fromid + "/" + Rto + "/" + toid + "/"
	url += bucket + "/" + objname
	file, err := os.Open(fqn)
	if err != nil {
		return fmt.Sprintf("Failed to open %q, err: %v", fqn, err)
	}
	defer file.Close()
	// calculate HASH for file and send it as part of header
	md5 := md5.New()
	md5hash, errstr = CalculateMD5(file, md5)
	if errstr != "" {
		return fmt.Sprintf("Failed to Calulate MD5Hash, err: %v", errstr)
	}
	_, err = file.Seek(0, 0)
	assert(err == nil, err)
	request, err := http.NewRequest(method, url, file)
	assert(err == nil, err)
	request.Header.Set("Content-HASH", md5hash)
	response, err := t.httpclient.Do(request)
	if err != nil {
		return fmt.Sprintf("Failed to copy %q from %s, err: %v", t.si.DaemonID, fqn, err)
	}
	if response != nil {
		ioutil.ReadAll(response.Body)
		response.Body.Close()
	}
	t.statsif.add("numsendfile", 1)
	return ""
}

//
// Cloud bucket + object => (local hashed path, fully qualified filename)
//
func (t *targetrunner) fqn(bucket, objname string) string {
	mpath := hrwMpath(bucket + "/" + objname)
	if t.islocalBucket(bucket) {
		return mpath + "/" + ctx.config.LocalBuckets + "/" + bucket + "/" + objname
	}
	return mpath + "/" + ctx.config.CloudBuckets + "/" + bucket + "/" + objname
}

//===========================
//
// control plane
//
//===========================

// "/"+Rversion+"/"+Rdaemon
func (t *targetrunner) daemonhdlr(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		t.httpdaeget(w, r)
	case http.MethodPut:
		t.httpdaeput(w, r)
	default:
		invalhdlr(w, r)
	}
	glog.Flush()
}

func (t *targetrunner) httpdaeput(w http.ResponseWriter, r *http.Request) {
	apitems := t.restAPIItems(r.URL.Path, 5)
	if apitems = t.checkRestAPI(w, r, apitems, 0, Rversion, Rdaemon); apitems == nil {
		return
	}
	// PUT '{Smap}' /v1/daemon/(syncsmap|rebalance)
	if len(apitems) > 0 && (apitems[0] == Rsyncsmap || apitems[0] == Rebalance) {
		t.httpdaeput_smap(w, r, apitems)
		return
	}
	// PUT '{lbmap}' /v1/daemon/localbuckets
	if len(apitems) > 0 && apitems[0] == Rsynclb {
		t.httpdaeput_lbmap(w, r, apitems)
		return
	}
	//
	// other PUT /daemon actions
	//
	var msg ActionMsg
	if t.readJSON(w, r, &msg) != nil {
		return
	}
	switch msg.Action {
	case ActSetConfig:
		if errstr := t.setconfig(msg.Name, msg.Value); errstr != "" {
			t.invalmsghdlr(w, r, errstr)
		}
		if msg.Name == "lru_enabled" && msg.Value == "false" {
			_, lruxact := t.xactinp.find(ActLRU)
			if lruxact != nil {
				if glog.V(3) {
					glog.Infof("Aborting LRU due to lru_enabled config change")
				}
				lruxact.abort()
			}
		}
	case ActShutdown:
		syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	default:
		s := fmt.Sprintf("Unexpected ActionMsg <- JSON [%v]", msg)
		t.invalmsghdlr(w, r, s)
	}
}

func (t *targetrunner) httpdaeput_smap(w http.ResponseWriter, r *http.Request, apitems []string) {
	curversion := t.smap.Version
	var newsmap *Smap
	if t.readJSON(w, r, &newsmap) != nil {
		return
	}
	if curversion == newsmap.Version {
		return
	}
	if curversion > newsmap.Version {
		glog.Errorf("Warning: attempt to downgrade Smap verion %d to %d", curversion, newsmap.Version)
		return
	}
	glog.Infof("%s: new Smap version %d (old %d)", apitems[0], newsmap.Version, curversion)

	// 1. check whether this target is present in the new Smap
	// 2. log
	// 3. nothing to rebalance if the new map is a strict subset of the old
	existentialQ, isSubset := false, true
	for id, si := range newsmap.Smap { // log
		if id == t.si.DaemonID {
			existentialQ = true
			glog.Infoln("target:", si, "<= self")
		} else {
			glog.Infoln("target:", si)
		}
		if _, ok := t.smap.Smap[id]; !ok {
			isSubset = false
		}
	}
	assert(existentialQ)

	t.smap = newsmap
	if apitems[0] == Rsyncsmap {
		return
	}
	if isSubset {
		glog.Infoln("nothing to rebalance: new Smap is a strict subset of the old")
		return
	}
	// xaction
	go t.runRebalance()
}

func (t *targetrunner) httpdaeput_lbmap(w http.ResponseWriter, r *http.Request, apitems []string) {
	curversion := t.lbmap.Version
	newlbmap := &lbmap{LBmap: make(map[string]string)}
	if t.readJSON(w, r, newlbmap) != nil {
		return
	}
	if curversion == newlbmap.Version {
		return
	}
	if curversion > newlbmap.Version {
		glog.Errorf("Warning: attempt to downgrade lbmap verion %d to %d", curversion, newlbmap.Version)
		return
	}
	glog.Infof("%s: new lbmap version %d (old %d)", apitems[0], newlbmap.Version, curversion)
	// destroylb
	for bucket := range t.lbmap.LBmap {
		_, ok := newlbmap.LBmap[bucket]
		if !ok {
			glog.Infof("Destroy local bucket %s", bucket)
			for mpath := range ctx.mountpaths {
				localbucketfqn := mpath + "/" + ctx.config.LocalBuckets + "/" + bucket
				if err := os.RemoveAll(localbucketfqn); err != nil {
					glog.Errorf("Failed to destroy local bucket dir %q, err: %v", localbucketfqn, err)
				}
			}
		}
	}
	t.lbmap = newlbmap
	for mpath := range ctx.mountpaths {
		for bucket := range t.lbmap.LBmap {
			localbucketfqn := mpath + "/" + ctx.config.LocalBuckets + "/" + bucket
			if err := CreateDir(localbucketfqn); err != nil {
				glog.Errorf("Failed to create local bucket dir %q, err: %v", localbucketfqn, err)
			}
		}
	}
	return
}

func (t *targetrunner) httpdaeget(w http.ResponseWriter, r *http.Request) {
	apitems := t.restAPIItems(r.URL.Path, 5)
	if apitems = t.checkRestAPI(w, r, apitems, 0, Rversion, Rdaemon); apitems == nil {
		return
	}
	var msg GetMsg
	if t.readJSON(w, r, &msg) != nil {
		return
	}
	var (
		jsbytes []byte
		err     error
	)
	switch msg.GetWhat {
	case GetWhatConfig:
		jsbytes, err = json.Marshal(ctx.config)
		assert(err == nil)
	case GetWhatSmap:
		jsbytes, err = json.Marshal(t.si)
		assert(err == nil, err)
	case GetWhatStats:
		rr := getstorstatsrunner()
		rr.Lock()
		jsbytes, err = json.Marshal(rr)
		rr.Unlock()
		assert(err == nil, err)
	default:
		s := fmt.Sprintf("Unexpected GetMsg <- JSON [%v]", msg)
		t.invalmsghdlr(w, r, s)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsbytes)
}

//==============================================================================
//
// target's utilities and helpers
//
//==============================================================================
func (t *targetrunner) islocalBucket(bucket string) bool {
	_, ok := t.lbmap.LBmap[bucket]
	return ok
}

func (t *targetrunner) testingFSPpaths() bool {
	return ctx.config.TestFSP.Count > 0
}

func (t *targetrunner) fqn2bckobj(fqn string) (bucket, objname string, ok bool) {
	fn := func(path string) bool {
		if strings.HasPrefix(fqn, path) {
			rempath := fqn[len(path):]
			items := strings.SplitN(rempath, "/", 2)
			bucket, objname = items[0], items[1]
			return true
		}
		return false
	}
	for mpath := range ctx.mountpaths {
		if fn(mpath + "/" + ctx.config.CloudBuckets + "/") {
			ok = len(objname) > 0
			return
		}
		if fn(mpath + "/" + ctx.config.LocalBuckets + "/") {
			assert(t.islocalBucket(bucket))
			ok = len(objname) > 0
			return
		}
	}
	return
}

func (t *targetrunner) fspath2mpath() {
	for fp := range ctx.config.FSpaths {
		if _, err := os.Stat(fp); err != nil {
			glog.Fatalf("FATAL: fspath %q does not exist, err: %v", fp, err)
		}
		statfs := syscall.Statfs_t{}
		if err := syscall.Statfs(fp, &statfs); err != nil {
			glog.Fatalf("FATAL: cannot statfs fspath %q, err: %v", fp, err)
		}

		mp := &mountPath{Path: fp, Fsid: statfs.Fsid}
		_, ok := ctx.mountpaths[mp.Path]
		assert(!ok)
		ctx.mountpaths[mp.Path] = mp
	}
}

// create local directories to test multiple fspaths
func (t *targetrunner) testCachepathMounts() {
	var instpath string
	if ctx.config.TestFSP.Instance > 0 {
		instpath = ctx.config.TestFSP.Root + strconv.Itoa(ctx.config.TestFSP.Instance) + "/"
	} else {
		// e.g. when docker
		instpath = ctx.config.TestFSP.Root
	}
	for i := 0; i < ctx.config.TestFSP.Count; i++ {
		var mpath string
		if ctx.config.TestFSP.Count > 1 {
			mpath = instpath + strconv.Itoa(i+1)
		} else {
			mpath = instpath[0 : len(instpath)-1]
		}
		if err := CreateDir(mpath); err != nil {
			glog.Fatalf("FATAL: cannot create test cache dir %q, err: %v", mpath, err)
			return
		}
		statfs := syscall.Statfs_t{}
		if err := syscall.Statfs(mpath, &statfs); err != nil {
			glog.Fatalf("FATAL: cannot statfs mpath %q, err: %v", mpath, err)
			return
		}
		mp := &mountPath{Path: mpath, Fsid: statfs.Fsid}
		_, ok := ctx.mountpaths[mp.Path]
		assert(!ok)
		ctx.mountpaths[mp.Path] = mp
	}
}

func (t *targetrunner) mpath2Fsid() (fsmap map[syscall.Fsid]string) {
	fsmap = make(map[syscall.Fsid]string, len(ctx.mountpaths))
	for _, mountpath := range ctx.mountpaths {
		mp2, ok := fsmap[mountpath.Fsid]
		if ok {
			if !t.testingFSPpaths() {
				glog.Fatalf("FATAL: duplicate FSID %v: mpath1 %q, mpath2 %q", mountpath.Fsid, mountpath.Path, mp2)
			}
			continue
		}
		fsmap[mountpath.Fsid] = mountpath.Path
	}
	return
}
