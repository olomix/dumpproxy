package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"
)

var listenAddr = flag.String("listen-addr", "localhost:8080", "listen address")
var upstreamAddr = flag.String(
	"upstream-addr", "localhost:80", "upstream address",
)
var dumpDir = flag.String("dir", "./", "directory to dump traffic")

var dialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	DualStack: true,
}

func dial(ctx context.Context, _, _ string) (net.Conn, error) {
	return dialer.DialContext(ctx, "tcp", *upstreamAddr)
}

func skipRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

var httpClient = http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: skipRedirect,
}

func proxy(w http.ResponseWriter, r *http.Request) {
	url := "http://" + r.Host + r.RequestURI
	cr, err := http.NewRequest(r.Method, url, r.Body)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer closeLogError(cr.Body)

	cr = cr.WithContext(r.Context())

	resp, err := httpClient.Do(cr)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer closeLogError(resp.Body)

	for header, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(header, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	bodySize, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Print(err)
		return
	}
	fNamePrefix, err := fname()
	if err != nil {
		log.Print(err)
	}
	_ = fNamePrefix

	log.Printf("%v %v %v %v %v", r.Host, resp.StatusCode, bodySize, r.URL, fNamePrefix)
}

func fname() (string, error) {
	datePrefix := path.Join(*dumpDir, time.Now().Format("2006-01-02-15-04-05-"))
	idx := 0
	var prefix string
	for {
		prefix = datePrefix + strconv.Itoa(idx)
		fname := prefix + ".request_headers"
		f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
		if err != nil {
			if os.IsExist(err) {
				idx++
				continue
			}
			panic(err)
		}
		return prefix, f.Close()
	}
}

func closeLogError(closer io.Closer) {
	if closer == nil {
		return
	}

	err := closer.Close()
	if err != nil {
		log.Print(err)
	}
}

func main() {
	flag.Parse()

	fileInfo, err := os.Stat(*dumpDir)
	if err != nil {
		panic(err)
	}
	if !fileInfo.IsDir() {
		panic(fmt.Sprintf("%v is not a directory", *dumpDir))
	}

	panic(http.ListenAndServe(*listenAddr, http.HandlerFunc(proxy)))
}
