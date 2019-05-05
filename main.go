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
	"strings"
	"time"
)

var listenAddr = flag.String("listen-addr", "localhost:8080", "listen address")
var upstreamAddr = flag.String(
	"upstream-addr", "localhost:80", "upstream address",
)
var dumpDir = flag.String("dir", "./", "directory to dump traffic")

const suffixReqHeaders = ".request_headers"
const suffixReqBody = ".request_body"
const suffixRespHeaders = ".response_headers"
const suffixRespBody = ".response_body"

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

	var (
		err         error
		fNamePrefix string
		statusCode  = 0
	)

	// Log request
	defer func() {
		log.Printf(
			"%v %v %v %v %v",
			r.Host,
			extractAddr(r.RemoteAddr),
			statusCode,
			r.URL,
			fNamePrefix,
		)
		if err != nil {
			log.Print(err)
		}
	}()

	fNamePrefix, err = fname()
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}

	var reqBodyFile *os.File
	reqBodyFile, err = os.Create(fNamePrefix + suffixReqBody)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}
	defer closeLogError(reqBodyFile)
	bodyReader := io.TeeReader(r.Body, reqBodyFile)

	var cr *http.Request
	cr, err = http.NewRequest(r.Method, url, bodyReader)
	if err != nil {
		statusCode = http.StatusBadGateway
		w.WriteHeader(statusCode)
		return
	}
	defer closeLogError(cr.Body)

	cr = cr.WithContext(r.Context())

	var reqHeadersFile *os.File
	reqHeadersFile, err = os.Create(fNamePrefix + suffixReqHeaders)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}
	defer closeLogError(reqHeadersFile)

	_, err = fmt.Fprintf(
		reqHeadersFile, "%v %v %v\n", r.Method, r.RequestURI, r.Proto,
	)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}

	for header, values := range r.Header {
		for _, value := range values {
			cr.Header.Add(header, value)
			_, err = fmt.Fprintf(reqHeadersFile, "%v: %v\n", header, value)
			if err != nil {
				statusCode = http.StatusInternalServerError
				w.WriteHeader(statusCode)
				return
			}
		}
	}

	var resp *http.Response
	resp, err = httpClient.Do(cr)
	if err != nil {
		statusCode = http.StatusBadGateway
		w.WriteHeader(statusCode)
		return
	}
	defer closeLogError(resp.Body)

	statusCode, err = processResponseHeaders(fNamePrefix, resp, w)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}

	if err = processResponseBody(fNamePrefix, resp.Body, w); err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		return
	}
}

func processResponseHeaders(
	dumpFilePrefix string,
	resp *http.Response,
	w http.ResponseWriter,
) (int, error) {
	respHeadersFile, err := os.Create(dumpFilePrefix + suffixRespHeaders)
	if err != nil {
		return 0, err
	}
	defer closeLogError(respHeadersFile)

	_, err = fmt.Fprintf(respHeadersFile, "%v\n", resp.Status)
	if err != nil {
		return 0, err
	}

	for header, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(header, value)
			_, err = fmt.Fprintf(respHeadersFile, "%v: %v\n", header, value)
			if err != nil {
				return 0, err
			}
		}
	}

	w.WriteHeader(resp.StatusCode)

	return resp.StatusCode, nil
}

func processResponseBody(
	dumpFilePrefix string,
	respBody io.Reader,
	w io.Writer,
) error {
	respBodyFile, err := os.Create(dumpFilePrefix + suffixRespBody)
	if err != nil {
		return err
	}
	defer closeLogError(respBodyFile)

	var buf = make([]byte, 16384)
	for {
		brk := false
		n, err := respBody.Read(buf)
		switch err {
		case io.EOF:
			brk = true
		case nil:
		default:
			return err
		}

		n2, err := respBodyFile.Write(buf[:n])
		if err != nil {
			return err
		}
		if n2 != n {
			panic("[assertion] n2 != n")
		}

		n3, err := w.Write(buf[:n])
		if err != nil {
			return err
		}
		if n3 != n {
			panic("[assertion] n3 != n")
		}
		if brk {
			break
		}
	}

	return nil
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

func extractAddr(addr string) string {
	idx := strings.IndexRune(addr, ':')
	if idx > 6 {
		return addr[:idx]
	}
	return addr
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
