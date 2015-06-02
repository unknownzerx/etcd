// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdmain

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"reflect"
	"time"

	"github.com/coreos/etcd/discovery"
	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/etcdhttp"
	"github.com/coreos/etcd/pkg/cors"
	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/coreos/etcd/pkg/osutil"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/proxy"
	"github.com/coreos/etcd/rafthttp"

	"github.com/coreos/etcd/Godeps/_workspace/src/github.com/coreos/pkg/capnslog"
)

type dirType string

var log = capnslog.NewPackageLogger("github.com/coreos/etcd", "etcdmain")

const (
	// the owner can make/remove files inside the directory
	privateDirMode = 0700
)

var (
	dirMember = dirType("member")
	dirProxy  = dirType("proxy")
	dirEmpty  = dirType("empty")
)

func Main2(args []string) {
	capnslog.SetFormatter(capnslog.NewStringFormatter(os.Stderr))
	cfg := NewConfig()
	err := cfg.Parse(args)
	if err != nil {
		log.Printf("error verifying flags, %v. See 'etcd -help'.", err)
		switch err {
		case errUnsetAdvertiseClientURLsFlag:
			log.Printf("When listening on specific address(es), this etcd process must advertise accessible url(s) to each connected client.")
		}
		os.Exit(1)
	}
	setupLogging(cfg)

	var stopped <-chan struct{}

	// TODO: check whether fields are set instead of whether fields have default value
	if cfg.name != defaultName && cfg.initialCluster == initialClusterFromName(defaultName) {
		cfg.initialCluster = initialClusterFromName(cfg.name)
	}

	if cfg.dir == "" {
		cfg.dir = fmt.Sprintf("%v.etcd", cfg.name)
		log.Printf("no data-dir provided, using default data-dir ./%s", cfg.dir)
	}

	which := identifyDataDirOrDie(cfg.dir)
	if which != dirEmpty {
		log.Printf("already initialized as %v before, starting as etcd %v...", which, which)
	}

	shouldProxy := cfg.isProxy() || which == dirProxy
	if !shouldProxy {
		stopped, err = startEtcd(cfg)
		if err == discovery.ErrFullCluster && cfg.shouldFallbackToProxy() {
			log.Printf("discovery cluster full, falling back to %s", fallbackFlagProxy)
			shouldProxy = true
		}
	}
	if shouldProxy {
		err = startProxy(cfg)
	}
	if err != nil {
		switch err {
		case discovery.ErrDuplicateID:
			log.Printf("member %q has previously registered with discovery service token (%s).", cfg.name, cfg.durl)
			log.Printf("But etcd could not find vaild cluster configuration in the given data dir (%s).", cfg.dir)
			log.Printf("Please check the given data dir path if the previous bootstrap succeeded")
			log.Printf("or use a new discovery token if the previous bootstrap failed.")
		default:
			log.Fatalf("%v", err)
		}
	}

	osutil.HandleInterrupts()

	<-stopped
}

func StopEtcd() {
	osutil.XStopEtcd()
}

func Main() {
	Main2(os.Args[1:])
	osutil.Exit(0)
}

// startEtcd launches the etcd server and HTTP handlers for client/server communication.
func startEtcd(cfg *config) (<-chan struct{}, error) {
	urlsmap, token, err := getPeerURLsMapAndToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("error setting up initial cluster: %v", err)
	}

	pt, err := transport.NewTimeoutTransport(cfg.peerTLSInfo, rafthttp.DialTimeout, rafthttp.ConnReadTimeout, rafthttp.ConnWriteTimeout)
	if err != nil {
		return nil, err
	}

	if !cfg.peerTLSInfo.Empty() {
		log.Printf("peerTLS: %s", cfg.peerTLSInfo)
	}
	plns := make([]net.Listener, 0)
	for _, u := range cfg.lpurls {
		var l net.Listener
		l, err = transport.NewTimeoutListener(u.Host, u.Scheme, cfg.peerTLSInfo, rafthttp.ConnReadTimeout, rafthttp.ConnWriteTimeout)
		if err != nil {
			return nil, err
		}

		urlStr := u.String()
		log.Print("listening for peers on ", urlStr)
		defer func() {
			if err != nil {
				l.Close()
				log.Print("stopping listening for peers on ", urlStr)
			}
		}()
		plns = append(plns, l)
	}

	if !cfg.clientTLSInfo.Empty() {
		log.Printf("clientTLS: %s", cfg.clientTLSInfo)
	}
	clns := make([]net.Listener, 0)
	for _, u := range cfg.lcurls {
		var l net.Listener
		l, err = transport.NewKeepAliveListener(u.Host, u.Scheme, cfg.clientTLSInfo)
		if err != nil {
			return nil, err
		}

		urlStr := u.String()
		log.Print("listening for client requests on ", urlStr)
		defer func() {
			if err != nil {
				l.Close()
				log.Print("stopping listening for client requests on ", urlStr)
			}
		}()
		clns = append(clns, l)
	}

	srvcfg := &etcdserver.ServerConfig{
		Name:                cfg.name,
		ClientURLs:          cfg.acurls,
		PeerURLs:            cfg.apurls,
		DataDir:             cfg.dir,
		SnapCount:           cfg.snapCount,
		MaxSnapFiles:        cfg.maxSnapFiles,
		MaxWALFiles:         cfg.maxWalFiles,
		InitialPeerURLsMap:  urlsmap,
		InitialClusterToken: token,
		DiscoveryURL:        cfg.durl,
		DiscoveryProxy:      cfg.dproxy,
		NewCluster:          cfg.isNewCluster(),
		ForceNewCluster:     cfg.forceNewCluster,
		Transport:           pt,
		TickMs:              cfg.TickMs,
		ElectionTicks:       cfg.electionTicks(),
	}
	var s *etcdserver.EtcdServer
	s, err = etcdserver.NewServer(srvcfg)
	if err != nil {
		return nil, err
	}
	s.Start()
	osutil.RegisterInterruptHandler(s.Stop)

	if cfg.corsInfo.String() != "" {
		log.Printf("cors = %s", cfg.corsInfo)
	}
	ch := &cors.CORSHandler{
		Handler: etcdhttp.NewClientHandler(s),
		Info:    cfg.corsInfo,
	}
	ph := etcdhttp.NewPeerHandler(s.Cluster(), s.RaftHandler())
	// Start the peer server in a goroutine
	for _, l := range plns {
		go func(l net.Listener) {
			err := serveHTTP(l, ph, 5*time.Minute)
			// I am ignoring the error 'closed listener'
			// TODO handle other errors..
			fmt.Println(err.Error())
		}(l)
	}
	// Start a client server goroutine for each listen address
	for _, l := range clns {
		go func(l net.Listener) {
			// read timeout does not work with http close notify
			// TODO: https://github.com/golang/go/issues/9524
			err := serveHTTP(l, ch, 0)
			fmt.Println(err.Error())
		}(l)
	}
	osutil.RegisterInterruptHandler(func() {
		for _, l := range append(plns, clns...) {
			l.Close()
			log.Infof("*** close listener: ", l.Addr().String())
		}
	})
	return s.StopNotify(), nil
}

// startProxy launches an HTTP proxy for client communication which proxies to other etcd nodes.
func startProxy(cfg *config) error {
	urlsmap, _, err := getPeerURLsMapAndToken(cfg)
	if err != nil {
		return fmt.Errorf("error setting up initial cluster: %v", err)
	}

	if cfg.durl != "" {
		s, err := discovery.GetCluster(cfg.durl, cfg.dproxy)
		if err != nil {
			return err
		}
		if urlsmap, err = types.NewURLsMap(s); err != nil {
			return err
		}
	}

	pt, err := transport.NewTransport(cfg.clientTLSInfo)
	if err != nil {
		return err
	}

	tr, err := transport.NewTransport(cfg.peerTLSInfo)
	if err != nil {
		return err
	}

	cfg.dir = path.Join(cfg.dir, "proxy")
	err = os.MkdirAll(cfg.dir, 0700)
	if err != nil {
		return err
	}

	var peerURLs []string
	clusterfile := path.Join(cfg.dir, "cluster")

	b, err := ioutil.ReadFile(clusterfile)
	switch {
	case err == nil:
		urls := struct{ PeerURLs []string }{}
		err := json.Unmarshal(b, &urls)
		if err != nil {
			return err
		}
		peerURLs = urls.PeerURLs
		log.Printf("proxy: using peer urls %v from cluster file ./%s", peerURLs, clusterfile)
	case os.IsNotExist(err):
		peerURLs = urlsmap.URLs()
		log.Printf("proxy: using peer urls %v ", peerURLs)
	default:
		return err
	}

	clientURLs := []string{}
	uf := func() []string {
		gcls, err := etcdserver.GetClusterFromRemotePeers(peerURLs, tr)
		// TODO: remove the 2nd check when we fix GetClusterFromPeers
		// GetClusterFromPeers should not return nil error with an invaild empty cluster
		if err != nil {
			log.Printf("proxy: %v", err)
			return []string{}
		}
		if len(gcls.Members()) == 0 {
			return clientURLs
		}
		clientURLs = gcls.ClientURLs()

		urls := struct{ PeerURLs []string }{gcls.PeerURLs()}
		b, err := json.Marshal(urls)
		if err != nil {
			log.Printf("proxy: error on marshal peer urls %s", err)
			return clientURLs
		}

		err = ioutil.WriteFile(clusterfile+".bak", b, 0600)
		if err != nil {
			log.Printf("proxy: error on writing urls %s", err)
			return clientURLs
		}
		err = os.Rename(clusterfile+".bak", clusterfile)
		if err != nil {
			log.Printf("proxy: error on updating clusterfile %s", err)
			return clientURLs
		}
		if !reflect.DeepEqual(gcls.PeerURLs(), peerURLs) {
			log.Printf("proxy: updated peer urls in cluster file from %v to %v", peerURLs, gcls.PeerURLs())
		}
		peerURLs = gcls.PeerURLs()

		return clientURLs
	}
	ph := proxy.NewHandler(pt, uf)
	ph = &cors.CORSHandler{
		Handler: ph,
		Info:    cfg.corsInfo,
	}

	if cfg.isReadonlyProxy() {
		ph = proxy.NewReadonlyHandler(ph)
	}
	// Start a proxy server goroutine for each listen address
	for _, u := range cfg.lcurls {
		l, err := transport.NewListener(u.Host, u.Scheme, cfg.clientTLSInfo)
		if err != nil {
			return err
		}

		host := u.Host
		go func() {
			log.Print("proxy: listening for client requests on ", host)
			log.Fatal(http.Serve(l, ph))
		}()
	}
	return nil
}

// getPeerURLsMapAndToken sets up an initial peer URLsMap and cluster token for bootstrap or discovery.
func getPeerURLsMapAndToken(cfg *config) (urlsmap types.URLsMap, token string, err error) {
	switch {
	case cfg.durl != "":
		urlsmap = types.URLsMap{}
		// If using discovery, generate a temporary cluster based on
		// self's advertised peer URLs
		urlsmap[cfg.name] = cfg.apurls
		token = cfg.durl
	case cfg.dnsCluster != "":
		var clusterStr string
		clusterStr, token, err = discovery.SRVGetCluster(cfg.name, cfg.dnsCluster, cfg.initialClusterToken, cfg.apurls)
		if err != nil {
			return nil, "", err
		}
		urlsmap, err = types.NewURLsMap(clusterStr)
	default:
		// We're statically configured, and cluster has appropriately been set.
		urlsmap, err = types.NewURLsMap(cfg.initialCluster)
		token = cfg.initialClusterToken
	}
	return urlsmap, token, err
}

// identifyDataDirOrDie returns the type of the data dir.
// Dies if the datadir is invalid.
func identifyDataDirOrDie(dir string) dirType {
	names, err := fileutil.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return dirEmpty
		}
		log.Fatalf("error listing data dir: %s", dir)
	}

	var m, p bool
	for _, name := range names {
		switch dirType(name) {
		case dirMember:
			m = true
		case dirProxy:
			p = true
		default:
			log.Printf("found invalid file/dir %s under data dir %s (Ignore this if you are upgrading etcd)", name, dir)
		}
	}

	if m && p {
		log.Fatal("invalid datadir. Both member and proxy directories exist.")
	}
	if m {
		return dirMember
	}
	if p {
		return dirProxy
	}
	return dirEmpty
}

func setupLogging(cfg *config) {
	capnslog.SetGlobalLogLevel(capnslog.INFO)
	if cfg.debug {
		capnslog.SetGlobalLogLevel(capnslog.DEBUG)
	}
	if cfg.logPkgLevels != "" {
		repoLog := capnslog.MustRepoLogger("github.com/coreos/etcd")
		settings, err := repoLog.ParseLogLevelConfig(cfg.logPkgLevels)
		if err != nil {
			log.Warningf("Couldn't parse log level string: %s, continuing with default levels", err.Error())
			return
		}
		repoLog.SetLogLevel(settings)
	}
}
