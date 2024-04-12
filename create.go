package main

import (
	"archive/tar"
  "encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

func create(self io.Reader, key []byte, out string, files []string, cd string) {
	if len(files) == 0 {
		die("no files to archive")
	}

	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		die("opening output file:", err)
	}

	_, err = io.Copy(f, self)
	if err != nil {
		die("writing stub to output file:", err)
	}

	_, err = f.Write(generateBoundary())
	if err != nil {
		die("writing boundary to output file:", err)
	}

	_, err = f.Write(generateRandomKey())
	if err != nil {
		die("writing key to output file:", err)
	}

  _, err = f.Write([]byte("\xef\xbe\xad\xde\xef\xbe\xad\xde"))
  if err != nil {
    die("writing placeholder for payload size", err)
  }

  offset, err := f.Seek(0, io.SeekCurrent)
  if err != nil {
    die("getting start position of payload:", err)
  }

	zWrt, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		die("creating zstd compressor:", err)
	}

	tarWrt := tar.NewWriter(zWrt)

	for _, file := range files {
		rootDir := os.DirFS(cd)
		file = filepath.Clean(file)
		// file may be a simple file or a directory, walkdir works for both
		fs.WalkDir(rootDir, file, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				die("opening input file", path, err)
			}
			if path == "." {
				return nil
			}
			debug("archiving", path)

			var hdr tar.Header
			hdr.Name = path

			info, err := d.Info()
			if err != nil {
				die("getting info about file:", path)
			}
			mode := info.Mode()
			hdr.Mode = int64(mode)

			switch mode.Type() {
			case fs.ModeDir:
				hdr.Typeflag = tar.TypeDir
			case fs.ModeSymlink:
				hdr.Typeflag = tar.TypeSymlink
				target, err := os.Readlink(filepath.Join(cd, path))
				if err != nil {
					die("getting target of symlink:", path)
				}
				hdr.Linkname = target
			case 0: // regular file
				hdr.Typeflag = tar.TypeReg
				hdr.Size = info.Size()
			default:
				die("unsupported file type:", path)
			}

			err = tarWrt.WriteHeader(&hdr)
			if err != nil {
				die("writing tar header of file:", path)
			}

			if mode.Type() == 0 {
				wf, err := os.Open(filepath.Join(cd, path))
				if err != nil {
					die("opening file:", path)
				}
				_, err = io.Copy(tarWrt, wf)
				if err != nil {
					die("writing file to tar:", path)
				}
				wf.Close()
			}

			return nil
		})
	}

	err = tarWrt.Close()
	if err != nil {
		die("closing tar:", err)
	}
	err = zWrt.Close()
	if err != nil {
		die("closing zstd:", err)
	}

  payload_end, err := f.Seek(0, io.SeekCurrent)
  if err != nil {
    die("getting offset of end of payload:", err)
  }

  _, err = f.Seek(offset-8, io.SeekStart)
  if err != nil {
    die("seek back to payload size place holder");
  }

  buffer := make([]byte,8)
  binary.LittleEndian.PutUint64(buffer, uint64(payload_end-offset))
  f.Write(buffer)

	err = f.Chmod(0755)
	if err != nil {
		die("making output file executable:", err)
	}
	err = f.Close()
	if err != nil {
		die("closing output file:", err)
	}
}
