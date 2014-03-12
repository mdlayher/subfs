package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/mdlayher/gosubsonic"
)

// subsonic stores the instance of the gosubsonic client
var subsonic gosubsonic.Client

// nameToDir maps a directory name to its SubDir
var nameToDir map[string]SubDir

// nameToFile maps a file name to its SubFile
var nameToFile map[string]SubFile

// host is the host of the Subsonic server
var host = flag.String("host", "", "Host of Subsonic server")

// user is the username to connect to the Subsonic server
var user = flag.String("user", "", "Username for the Subsonic server")

// password is the password to connect to the Subsonic server
var password = flag.String("password", "", "Password for the Subsonic server")

// mount is the path where subfs will be mounted
var mount = flag.String("mount", "", "Path where subfs will be mounted")

func main() {
	// Parse command line flags
	flag.Parse()

	// Open connection to Subsonic
	sub, err := gosubsonic.New(*host, *user, *password)
	if err != nil {
		log.Fatalf("Could not connect to Subsonic server: %s", err.Error())
	}

	// Store subsonic client for global use
	subsonic = *sub

	// Initialize lookup maps
	nameToDir = map[string]SubDir{}
	nameToFile = map[string]SubFile{}

	// Attempt to mount filesystem
	c, err := fuse.Mount(*mount)
	if err != nil {
		log.Fatalf("Could not mount subfs at %s: %s", *mount, err.Error())
	}

	// Serve the FUSE filesystem
	log.Printf("subfs: %s@%s -> %s", *user, *host, *mount)
	go fs.Serve(c, SubFS{})

	// Wait for termination singals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	signal.Notify(sigChan, syscall.SIGTERM)
	for sig := range sigChan {
		log.Println("subfs: caught signal:", sig)
		break
	}

	// Unmount the FUSE filesystem
	if err := fuse.Unmount(*mount); err != nil {
		log.Fatalf("Could not unmount subfs at %s: %s", *mount, err.Error())
	}

	// Close the FUSE filesystem
	if err := c.Close(); err != nil {
		log.Fatalf("Could not close subfs: %s", err.Error())
	}

	return
}

// SubFS represents the root of the filesystem
type SubFS struct{}

// Root is called to get the root directory node of this filesystem
func (fs SubFS) Root() (fs.Node, fuse.Error) {
	return &SubDir{RelPath: ""}, nil
}

// SubDir represents a directory in the filesystem
type SubDir struct {
	ID      int64
	RelPath string
}

// Attr retrives the attributes for this SubDir
func (SubDir) Attr() fuse.Attr {
	return fuse.Attr{
		Mode: os.ModeDir | 0555,
	}
}

// ReadDir returns a list of directory entries depending on the current path
func (d SubDir) ReadDir(intr fs.Intr) ([]fuse.Dirent, fuse.Error) {
	// List of directory entries to return
	directories := make([]fuse.Dirent, 0)

	// If at root of filesystem, fetch indexes
	if d.RelPath == "" {
		index, err := subsonic.GetIndexes(-1, -1)
		if err != nil {
			log.Printf("Failed to retrieve indexes: %s", err.Error())
			return nil, fuse.ENOENT
		}

		// Iterate indices
		for _, i := range index {
			// Iterate all artists
			for _, a := range i.Artist {
				// Map artist's name to directory
				nameToDir[a.Name] = SubDir{
					ID:      a.ID,
					RelPath: "",
				}

				// Create a directory entry
				dir := fuse.Dirent{
					Name: a.Name,
					Type: fuse.DT_Dir,
				}

				// Append entry
				directories = append(directories, dir)
			}
		}
	} else {
		// Get this directory's contents
		content, err := subsonic.GetMusicDirectory(d.ID)
		if err != nil {
			log.Printf("Failed to retrieve directory %d: %s", d.ID, err.Error())
			return nil, fuse.ENOENT
		}

		// Iterate all returned directories
		for _, dir := range content.Directories {
			// Create a directory entry
			entry := fuse.Dirent{
				Name: dir.Title,
				Type: fuse.DT_Dir,
			}

			// Add SubDir directory to lookup map
			nameToDir[dir.Title] = SubDir{
				ID:      dir.ID,
				RelPath: d.RelPath + dir.Title,
			}

			// Append to list
			directories = append(directories, entry)
		}

		// Iterate all returned media
		for _, m := range content.Media {
			// Predefined media filename format
			mediaFormat := fmt.Sprintf("%02d - %s - %s.%s", m.Track, m.Artist, m.Title, m.Suffix)

			// Create a directory entry
			dir := fuse.Dirent{
				Name: mediaFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       m.ID,
				Created:  m.Created,
				FileName: mediaFormat,
				Size:     m.Size,
			}

			// Append to list
			directories = append(directories, dir)
		}
	}

	// Return all directory entries
	return directories, nil
}

// Lookup scans the current directory for matching files or directories
func (d SubDir) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	// Lookup directory by name
	if dir, ok := nameToDir[name]; ok {
		dir.RelPath = name + "/"
		return dir, nil
	}

	// Lookup file by name
	if f, ok := nameToFile[name]; ok {
		return f, nil
	}

	// File not found
	return nil, fuse.ENOENT
}

// SubFile represents a file in Subsonic library
type SubFile struct {
	ID       int64
	Created  time.Time
	FileName string
	Size     int64
}

// Attr returns file attributes (all files read-only)
func (s SubFile) Attr() fuse.Attr {
	return fuse.Attr{
		Mode:  0444,
		Mtime: s.Created,
		Size:  uint64(s.Size),
	}
}

// ReadAll opens a file stream from Subsonic and returns the resulting bytes
func (s SubFile) ReadAll(intr fs.Intr) ([]byte, fuse.Error) {
	// Byte stream to return data
	byteChan := make(chan []byte)

	// Fetch file in background
	go func() {
		// Open stream
		log.Printf("Opening stream: [%d] %s", s.ID, s.FileName)
		stream, err := subsonic.Stream(s.ID, nil)
		if err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Read in stream
		file, err := ioutil.ReadAll(stream)
		if err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Close stream
		if err := stream.Close(); err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Return bytes
		log.Printf("Closing stream: [%d] %s", s.ID, s.FileName)
		byteChan <- file
	}()

	// Wait for an event on read
	select {
	// Byte stream channel
	case stream := <-byteChan:
		return stream, nil
	// Interrupt channel
	case <-intr:
		return nil, fuse.EINTR
	}
}
