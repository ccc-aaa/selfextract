package main

import (
	"archive/tar"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/google/shlex"
)

const keyFileName = ".selfextract.key"

type selfExtractor struct {
	extractDir  string
	skipExtract bool
	tempDir     bool
	payload     io.Reader
	key         []byte
	exitCode    chan int
}

func checkExecutable(path string) bool {
	mntinf, err := os.Open("/proc/mounts")
	defer mntinf.Close()

	if err != nil {
		debug("Failed to retrieve mountinfo, assume it's not.")
		return false
	}

	raw, err := io.ReadAll(mntinf)

	if err != nil {
		debug("Failed to retrieve mountinfo, assume it's not.")
		return false
	}

	data := string(raw[:])
	mntp := ""
	noexec := false

	for _, entry := range strings.Split(data, "\n") {
		fields := strings.Split(entry, " ")
		if len(fields) < 2 {
			continue
		}

		mountpoint, options := fields[1], fields[3]

		if len(mountpoint) > len(path) {
			continue
		}

		if mountpoint == path { // exact match, early return
			mntp = mountpoint
			noexec = slices.Contains(strings.Split(options, ","), "noexec")
			break
		}

		if !strings.HasSuffix(mountpoint, "/") {
			mountpoint += "/"
		}

		if strings.HasPrefix(path, mountpoint) {
			if len(mntp) < len(mountpoint) {
				mntp = mountpoint
				noexec = slices.Contains(strings.Split(options, ","), "noexec")
			}
		}
	}

	debug(path, mntp, noexec)

	return !noexec
}

func extract(payload io.Reader, key []byte) {
	se := selfExtractor{
		payload:  payload,
		key:      key,
		exitCode: make(chan int),
	}
	se.setupSignals()
	se.prepareExtractDir()
	se.extract()
	go se.startup()
	exit := <-se.exitCode
	se.cleanup()
	os.Exit(exit)
}

func (se *selfExtractor) setupSignals() {
	grace := 10 * time.Second
	if graceStr := os.Getenv(EnvGraceTimeout); graceStr != "" {
		graceFl, err := strconv.ParseFloat(graceStr, 32)
		if err == nil && graceFl >= 0 {
			grace = time.Duration(graceFl) * time.Second
		}
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGABRT, syscall.SIGQUIT)

	go func() {
		<-c
		debug("got signal, waiting for grace timeout before exiting")
		if grace != 0 {
			time.Sleep(grace)
		}
		se.exitCode <- 2
	}()
}

func (se *selfExtractor) getTarReader() *tar.Reader {
	zRdr, err := zstd.NewReader(se.payload)
	if err != nil {
		die("creating zstd reader:", err)
	}

	return tar.NewReader(zRdr)
}

func (se *selfExtractor) getCwd() (string) {
	exe, err := os.Executable()
	if err != nil {
		die("Failed to retrieve current executable, refuse to continue.")
	}

	return filepath.Dir(exe)
}

func (se *selfExtractor) generateExtractDir() (string, error) {
	tmp := os.TempDir()
	pwd := se.getCwd()

	if checkExecutable(tmp) {
		return os.MkdirTemp("", "selfextract")
	}

	if checkExecutable(pwd) {
		return os.MkdirTemp(pwd, "selfextract")
	}

	die("cannot find suitable directory for execution, please assign one manually.")

	return "", errors.New("No suitable temp dir found.")
}

func (se *selfExtractor) prepareExtractDir() {
	extractDir := os.Getenv(EnvDir)

	if extractDir == "" {
		var err error
		se.extractDir, err = se.generateExtractDir()
		if err != nil {
			die("creating temporary extraction directory:", err)
		}
		se.tempDir = true
		return
	}

	se.extractDir = extractDir

	stat, err := os.Stat(extractDir)
	// if there's an error, we'll assume that it's because the directory
	// doesn't exist, so we create it
	if err != nil {
		err = os.MkdirAll(extractDir, 0755)
		if err != nil {
			die("creating extraction directory:", err)
		}
		return
	}

	if !stat.IsDir() {
		die("extraction directory not a directory")
	}

	// At this point, we know extractDir is a pre-existing directory.
	// To continue, we request that it's either:
	// - empty
	// - containing a key file (and possibly other files)
	// If it's either, we assume it's safe to use it, possibly erasing the files
	// it contains. If it's neither, the extract dir path may have been set to
	// an existing non-empty directory by error, so as a safeguard we abort.

	entries, err := os.ReadDir(extractDir)
	if err != nil {
		die("listing extraction dir:", err)
	}
	if len(entries) == 0 {
		return
	}

	keyFile, err := os.Open(filepath.Join(extractDir, keyFileName))
	if err != nil {
		die("opening key file (extraction dir must be empty or contain a valid key file):", err)
	}
	defer keyFile.Close()

	keyData, err := io.ReadAll(keyFile)
	if err != nil {
		die("reading key file (extraction dir must be empty or contain a valid key file):", err)
	}

	if hex.EncodeToString(se.key) == strings.TrimSpace(string(keyData)) {
		debug("extraction dir has matching key")
		se.skipExtract = true
		return
	}

	debug("key doesn't match, cleaning extraction dir")
	err = cleanupDir(extractDir)
	if err != nil {
		die("cleaning extraction dir:", err)
	}
}

// cleanupDir removes the contents of a directory but not the directory itself
func cleanupDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		err := os.RemoveAll(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
	}
	return nil
}

func createFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func cleanupAndDie(dir string, v ...interface{}) {
	err := cleanupDir(dir)
	if err != nil {
		die(append([]interface{}{"got error:", err, "while cleaning up after:"}, v...))
	}
	die(v...)
}

func (se *selfExtractor) extract() {
	debug("using extraction dir", se.extractDir)

	if se.skipExtract {
		debug("skipping extraction")
		return
	}

	tarRdr := se.getTarReader()

	for {
		hdr, err := tarRdr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			die("reading embedded tar:", err)
		}

		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		pathName := filepath.Join(se.extractDir, name)
		switch hdr.Typeflag {
		case tar.TypeReg:
			debug("extracting file", name, "of size", hdr.Size)
			f, err := createFile(pathName)
			if err != nil {
				cleanupAndDie(se.extractDir, "creating file:", err)
			}

			_, err = io.Copy(f, tarRdr)
			if err != nil {
				cleanupAndDie(se.extractDir, "writing file:", err)
			}

			err = f.Chmod(os.FileMode(hdr.Mode))
			if err != nil {
				cleanupAndDie(se.extractDir, "setting mode of file:", err)
			}

			f.Close()
		case tar.TypeDir:
			debug("creating directory", name)
			// We choose to disregard directory permissions and use a default
			// instead. Custom permissions (e.g. read-only directories) are
			// complex to handle, both when extracting and also when cleaning
			// up the directory.
			err := os.Mkdir(pathName, 0755)
			if err != nil {
				cleanupAndDie(se.extractDir, "creating directory", err)
			}
		case tar.TypeSymlink:
			debug("creating symlink", name)
			err := os.Symlink(hdr.Linkname, pathName)
			if err != nil {
				cleanupAndDie(se.extractDir, "creating symlink", err)
			}
		default:
			cleanupAndDie(se.extractDir, "unsupported file type in tar", hdr.Typeflag)
		}
	}

	se.createKeyFile()
}

func (se *selfExtractor) createKeyFile() {
	f, err := os.Create(filepath.Join(se.extractDir, keyFileName))
	if err != nil {
		die("creating key file:", err)
	}
	_, err = f.WriteString(hex.EncodeToString(se.key))
	if err != nil {
		die("writing key file:", err)
	}
	err = f.Close()
	if err != nil {
		die("closing key file:", err)
	}
}

func (se *selfExtractor) startup() {
	if isTruthy(os.Getenv(EnvExtractOnly)) {
		debug("extract only mode, skipping startup")
		se.exitCode <- 0
		return
	}

  cmdline := "selfextract_cmdline"

	os.Setenv(EnvDir, se.extractDir)

	debug("try using cmdline file", cmdline)
	cmdlinePath := filepath.Join(se.extractDir, cmdline)
  _, err := os.Stat(cmdlinePath)
  if err == nil {
    se.runCmdline(cmdlinePath)
    return
  }
	die("failed to find cmdline file, quitting...")
}

func (se *selfExtractor) runCmdline(path string) {
  cmdfile,err := os.Open(path)
  if err != nil {
    debug("failed to open cmdfile with error: ", err)
    se.exitCode <- 1
    return
  }

  cmdbytes,err := io.ReadAll(cmdfile)
  if err != nil {
    debug("failed to read cmdfile with error: ", err)
    se.exitCode <- 1
    return
  }

  defer cmdfile.Close()
  cmdline := strings.TrimSpace(string(cmdbytes[:]))
  cmdline = strings.ReplaceAll(cmdline, "__EXTRACT_DIR__", se.extractDir)
  args, err := shlex.Split(cmdline)
  if err != nil {
    debug("failed to parse cmdline arguments", err)
    se.exitCode <- 1
    return
  }

  args = append(args, os.Args[1:]...)
  cmd := exec.Command(args[0], args[1:]...)
  cmd.Stdin = os.Stdin
  cmd.Stderr = os.Stderr
  cmd.Stdout = os.Stdout
  err = cmd.Run()
  if err != nil {
  	debug("cmdline ended with error:", err)
  	var ex *exec.ExitError
  	if errors.As(err, &ex) {
  		se.exitCode <- ex.ExitCode()
  	} else {
  		se.exitCode <- 1
  	}
  	return
  }
  se.exitCode <- 0
}

func (se *selfExtractor) cleanup() {
	if se.tempDir {
		debug("removing extraction dir")
		os.RemoveAll(se.extractDir)
	}
}
