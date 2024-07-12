package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

var verbose bool

const (
	EnvVerbose      = "SELFEXTRACT_VERBOSE"
	EnvDir          = "SELFEXTRACT_DIR"
	EnvExtractOnly  = "SELFEXTRACT_EXTRACT_ONLY"
	EnvGraceTimeout = "SELFEXTRACT_GRACE_TIMEOUT"
)

func init() {
	verbose = isTruthy(os.Getenv(EnvVerbose))
}

func main() {
	self := openSelf()
	defer self.Close()

	payload, key := parseSelf(self)

	if payload != nil {
		extract(payload, key)
		return
	}

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "%s [OPTION...] FILE ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	createName := flag.String("f", "selfextract.out", "name of the archive to create")
	changeDir := flag.String("C", ".", "change dir before archiving files, only affects input files")
	verboseFlg := flag.Bool("v", false, "verbose output")
	flag.Parse()
	verbose = verbose || *verboseFlg

	self.Seek(0, os.SEEK_SET)
	create(self, key, *createName, flag.Args(), *changeDir)
}

func debug(v ...interface{}) {
	if verbose {
		v = append([]interface{}{"selfextract:"}, v...)
		log.Println(v...)
	}
}

func die(v ...interface{}) {
	v = append([]interface{}{"selfextract: FATAL:"}, v...)
	log.Fatalln(v...)
}

func isTruthy(s string) bool {
	switch strings.ToLower(s) {
	case "y", "yes", "true", "1":
		return true
	default:
		return false
	}
}

func generateBoundary() []byte {
	h := sha512.Sum512([]byte("boundary"))
	return h[:]
}

const keyLength = 16

func generateRandomKey() []byte {
	buf := make([]byte, keyLength)
	_, err := rand.Read(buf)
	if err != nil {
		die("generating random key:", err)
	}
	return buf
}

// maxBoundaryOffset is the offset at which we stop looking for a boundary,
// it's just a failsafe mechanism against big, corrupted archives. We set it to
// a value much bigger than the expected size of the compiled stub.
const maxBoundaryOffset = 100e6 // 100 MB

// efficient read size
const scanBlockSize = 128*1024 // 128 KB

func openSelf() (io.ReadSeekCloser) {
 	t := time.Now()
	exePath, err := os.Executable()
	if err != nil {
		panic(err)
	}
	self, err := os.Open(exePath)
	if err != nil {
		die("opening itself:", exePath, err)
	}
	debug("opened itself in", time.Since(t))
	return self
}

func parseSelf(self io.ReadSeeker) (io.Reader, []byte) {
	bdyOff := 0
	bufFull := false
	buf := make([]byte, scanBlockSize)
	boundary := generateBoundary()
	t := time.Now()

	for {
		n, err := self.Read(buf)

		if err == io.EOF {
			bufFull = true
			break
		}

		if err != nil {
			die("reading itself:", err)
		}

		bOff := bytes.Index(buf[:n], boundary)
		if bOff >= 0 {
			bdyOff += bOff
			break
		}
		bdyOff += scanBlockSize
	}
	debug("boundary search completed in", time.Since(t))

	if bufFull {
		debug("cannot found boundary within threshold")
		return nil, nil
	}

	debug("boundary found at", bdyOff)

	self.Seek(int64(bdyOff+len(boundary)), os.SEEK_SET)
	buf = make([]byte, keyLength+8)
	_, err := self.Read(buf)
	if err != nil {
		die("failed to read additional data from executable", err)
	}

	key := buf[:keyLength]
	rawValue := binary.LittleEndian.Uint64(buf[keyLength:][:8])

	if rawValue == 0xdeadbeefdeadbeef {
		die("Invalid archive size.")
	}

	payloadSize := int64(rawValue)
	reader := io.LimitReader(self, payloadSize)

	debug("Payload size:", payloadSize)

	return reader, key
}
