// Build only the client into versioned, checksummed release archives.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	version := flag.String("version", "", "release version, e.g. v0.1.0")
	commit := flag.String("commit", "", "full source commit SHA")
	out := flag.String("out", "dist", "new output directory")
	flag.Parse()
	if flag.NArg() != 0 || !versionPattern.MatchString(*version) || !commitPattern.MatchString(*commit) {
		return errors.New("a valid --version and full --commit SHA are required")
	}
	// Never merge releases or delete an existing output directory.
	if err := os.Mkdir(*out, 0755); err != nil {
		return err
	}
	built := time.Now().UTC().Format(time.RFC3339)
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	goexe := filepath.Join(runtime.GOROOT(), "bin", "go")
	targets := [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}
	checksums := []string{}
	for _, target := range targets {
		temp, err := os.MkdirTemp("", "kioku-release-")
		if err != nil {
			return err
		}
		err = func() error {
			defer os.RemoveAll(temp)
			binary := filepath.Join(temp, "kioku")
			ld := "-s -w -X github.com/mash/kioku/internal/buildinfo.Version=" + *version + " -X github.com/mash/kioku/internal/buildinfo.Commit=" + *commit + " -X github.com/mash/kioku/internal/buildinfo.BuiltAt=" + built
			cmd := exec.Command(goexe, "build", "-trimpath", "-buildvcs=false", "-ldflags", ld, "-o", binary, "./cmd/kioku")
			cmd.Dir = root
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") && !strings.HasPrefix(entry, "CGO_ENABLED=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			if target[0] == runtime.GOOS && target[1] == runtime.GOARCH {
				result, err := exec.Command(binary, "version").Output()
				if err != nil {
					return err
				}
				var info map[string]string
				if err = json.Unmarshal(result, &info); err != nil || info["version"] != *version || info["commit"] != *commit {
					return errors.New("built client version check failed")
				}
			}
			name := "kioku_" + *version + "_" + target[0] + "_" + target[1] + ".tar.gz"
			archive := filepath.Join(*out, name)
			if err := pack(binary, archive); err != nil {
				return err
			}
			f, err := os.Open(archive)
			if err != nil {
				return err
			}
			defer f.Close()
			h := sha256.New()
			if _, err = io.Copy(h, f); err != nil {
				return err
			}
			checksums = append(checksums, hex.EncodeToString(h.Sum(nil))+"  "+name)
			fmt.Println(name)
			return nil
		}()
		if err != nil {
			return err
		}
	}
	if err = os.WriteFile(filepath.Join(*out, "SHA256SUMS.txt"), []byte(strings.Join(checksums, "\n")+"\n"), 0644); err != nil {
		return err
	}
	notes := "# Kioku CLI " + *version + "\n\nClient binaries for macOS and Linux (amd64/arm64).\n\nVerify the archive against SHA256SUMS.txt, extract `kioku`, and run `kioku version`. macOS users can verify with `shasum -a 256 -c SHA256SUMS.txt`; Linux users can use `sha256sum -c SHA256SUMS.txt` after downloading the archives being verified.\n\nThe CLI requires a Kioku server and an authorized account. The archives contain only the client binary; they contain no server binary, credentials or production configuration.\n\nSource revision: `" + *commit + "`.\n"
	return os.WriteFile(filepath.Join(*out, "RELEASE_NOTES.md"), []byte(notes), 0644)
}
func pack(binary, dest string) (err error) {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
	}()
	gz := gzip.NewWriter(f)
	defer func() {
		if e := gz.Close(); err == nil {
			err = e
		}
	}()
	tw := tar.NewWriter(gz)
	defer func() {
		if e := tw.Close(); err == nil {
			err = e
		}
	}()
	source, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: "kioku", Mode: 0755, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, source)
	return err
}
