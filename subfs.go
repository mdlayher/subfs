package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
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

// fileCache maps a file name to its file pointer
var fileCache map[string]os.File

// cacheTotal is the total size of local files in the cache
var cacheTotal int64

// streamMap maps a fileID to a channel containing a file stream
var streamMap map[int64]chan []byte

// host is the host of the Subsonic server
var host = flag.String("host", "", "Host of Subsonic server")

// user is the username to connect to the Subsonic server
var user = flag.String("user", "", "Username for the Subsonic server")

// password is the password to connect to the Subsonic server
var password = flag.String("password", "", "Password for the Subsonic server")

// mount is the path where subfs will be mounted
var mount = flag.String("mount", "", "Path where subfs will be mounted")

// cacheSize is the maximum size of the local file cache in megabytes
var cacheSize = flag.Int64("cache", 100, "Size of the local file cache, in megabytes")

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

	// Initialize file cache
	fileCache = map[string]os.File{}
	cacheTotal = 0

	// Initialize stream map
	streamMap = map[int64]chan []byte{}

	// Attempt to mount filesystem
	c, err := fuse.Mount(*mount)
	if err != nil {
		log.Fatalf("Could not mount subfs at %s: %s", *mount, err.Error())
	}

	// Serve the FUSE filesystem
	log.Printf("subfs: %s@%s -> %s [cache: %d MB]", *user, *host, *mount, *cacheSize)
	go fs.Serve(c, SubFS{})

	// Wait for termination singals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	signal.Notify(sigChan, syscall.SIGTERM)
	for sig := range sigChan {
		log.Println("subfs: caught signal:", sig)
		break
	}

	// Purge all cached files
	for _, f := range fileCache {
		// Close file
		if err := f.Close(); err != nil {
			log.Println(err)
		}

		// Remove file
		if err := os.Remove(f.Name()); err != nil {
			log.Println(err)
		}
	}

	log.Printf("subfs: removed %d cached file(s)", len(fileCache))

	// Attempt to unmount the FUSE filesystem
	retry := 3
	for i := 0; i < retry+1; i++ {
		// Wait between attempts
		if i > 0 {
			<-time.After(time.Second * 3)
		}

		// Try unmount
		if err := fuse.Unmount(*mount); err != nil {
			// Force exit on last attempt
			if i == retry {
				log.Printf("subfs: could not unmount %s, halting!", *mount)
				os.Exit(1)
			}

			log.Printf("subfs: could not unmount %s, retrying %d of %d...", *mount, i+1, retry)
		} else {
			break
		}
	}

	// Close the FUSE filesystem
	if err := c.Close(); err != nil {
		log.Fatalf("Could not close subfs: %s", err.Error())
	}

	log.Printf("subfs: done!")
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

		// Check for available cover art IDs
		coverArt := make([]int64, 0)

		// Check if an ID is unique to a slice of IDs
		unique := func(id int64, slice []int64) bool {
			// Iterate the slice
			for _, item := range slice {
				// If there's a match, not unique
				if id == item {
					return false
				}
			}

			// No matches, unique item
			return true
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

			// Check for cover art
			if unique(dir.CoverArt, coverArt) {
				coverArt = append(coverArt, dir.CoverArt)
			}

			// Append to list
			directories = append(directories, entry)
		}

		// List of bad characters which should be replaced in filenames
		badChars := []string{"/", "\\"}

		// Iterate all returned audio
		for _, a := range content.Audio {
			// Predefined audio filename format
			audioFormat := fmt.Sprintf("%02d - %s - %s.%s", a.Track, a.Artist, a.Title, a.Suffix)

			// Check for any characters which may cause trouble with filesystem display
			for _, b := range badChars {
				audioFormat = strings.Replace(audioFormat, b, "_", -1)
			}

			// Create a directory entry
			dir := fuse.Dirent{
				Name: audioFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       a.ID,
				Created:  a.Created,
				FileName: audioFormat,
				Size:     a.Size,
				IsVideo:  false,
			}

			// Check for cover art
			if unique(a.CoverArt, coverArt) {
				coverArt = append(coverArt, a.CoverArt)
			}

			// Append to list
			directories = append(directories, dir)
		}

		// Iterate all returned video
		for _, v := range content.Video {
			// Predefined video filename format
			videoFormat := fmt.Sprintf("%s.%s", v.Title, v.Suffix)

			// Check for any characters which may cause trouble with filesystem display
			for _, b := range badChars {
				videoFormat = strings.Replace(videoFormat, b, "_", -1)
			}

			// Create a directory entry
			dir := fuse.Dirent{
				Name: videoFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       v.ID,
				Created:  v.Created,
				FileName: videoFormat,
				Size:     v.Size,
				IsVideo:  true,
			}

			// Check for cover art
			if unique(v.CoverArt, coverArt) {
				coverArt = append(coverArt, v.CoverArt)
			}

			// Append to list
			directories = append(directories, dir)
		}

		// Iterate all cover art
		for _, c := range coverArt {
			coverArtFormat := fmt.Sprintf("%d.jpg", c)

			// Create a directory entry
			dir := fuse.Dirent{
				Name: coverArtFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       c,
				FileName: coverArtFormat,
				IsArt:    true,
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
	IsArt    bool
	IsVideo  bool
	Size     int64
}

// Attr returns file attributes (all files read-only)
func (s SubFile) Attr() fuse.Attr {
	return fuse.Attr{
		Mode:  0644,
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
		// Check for file in cache
		if cFile, ok := fileCache[s.FileName]; ok {
			// Check for empty file, meaning the cached file got wiped out
			buf, err := ioutil.ReadFile(cFile.Name())
			if len(buf) == 0 && strings.Contains(err.Error(), "no such file or directory") {
				// Purge item from cache
				log.Printf("Cache missing: [%d] %s", s.ID, s.FileName)
				delete(fileCache, s.FileName)
				cacheTotal = atomic.AddInt64(&cacheTotal, -1*s.Size)

				// Print some cache metrics
				cacheUse := float64(cacheTotal) / 1024 / 1024
				cacheDel := float64(s.Size) / 1024 / 1024
				log.Printf("Cache use: %0.3f / %d.000 MB (-%0.3f MB)", cacheUse, *cacheSize, cacheDel)

				// Close file handle
				if err := cFile.Close(); err != nil {
					log.Println(err)
				}
			} else {
				// Return cached file
				byteChan <- buf
				close(byteChan)
				return
			}
		}

		// Check for pre-existing stream in progress, so that multiple clients can receive it without
		// requesting the stream multiple times.  Yeah concurrency!
		if streamChan, ok := streamMap[s.ID]; ok {
			// Wait for stream to be ready, and return it
			byteChan <- <-streamChan
			close(byteChan)
			return
		}

		// Generate a channel for clients wishing to wait on this stream
		streamMap[s.ID] = make(chan []byte, 0)

		// Open stream, depending on if item is audio, video, or art
		var stream io.ReadCloser

		// Item is art
		if s.IsArt {
			log.Printf("Opening art stream: [%d] %s", s.ID, s.FileName)

			// Get cover art stream
			out, err := subsonic.GetCoverArt(s.ID, -1)
			if err != nil {
				log.Println(err)
				byteChan <- nil
				close(byteChan)
				return
			}

			// Output stream
			stream = out
		} else {
			// Else, item is audio or video

			// Stream options, for extra options
			var streamOptions gosubsonic.StreamOptions
			if s.IsVideo {
				// Item is video
				streamOptions = gosubsonic.StreamOptions{
					Size: "1280x720",
				}

				log.Printf("Opening video stream: [%d] %s [%s]", s.ID, s.FileName, streamOptions.Size)
			} else {
				// Item is audio
				log.Printf("Opening audio stream: [%d] %s", s.ID, s.FileName)
			}

			// Get media file stream
			out, err := subsonic.Stream(s.ID, &streamOptions)
			if err != nil {
				log.Println(err)
				byteChan <- nil
				close(byteChan)
				return
			}

			// Output stream
			stream = out
		}

		// Read in stream
		file, err := ioutil.ReadAll(stream)
		if err != nil {
			log.Println(err)
			byteChan <- nil
			close(byteChan)
			return
		}

		// Calculate art size upon retrieval
		if s.IsArt {
			s.Size = int64(len(file))
		}

		// Close stream
		if err := stream.Close(); err != nil {
			log.Println(err)
			byteChan <- nil
			close(byteChan)
			return
		}

		// Return bytes
		log.Printf("Closing stream: [%d] %s", s.ID, s.FileName)
		byteChan <- file
		close(byteChan)

		// Attempt to return bytes to others waiting, remove this stream
		go func() {
			// Time out after waiting for 10 seconds
			select {
			case streamMap[s.ID] <- file:
			case <-time.After(time.Second * 10):
			}

			// Remove stream from map
			close(streamMap[s.ID])
			delete(streamMap, s.ID)
		}()

		// Check for maximum cache size
		if cacheTotal > *cacheSize*1024*1024 {
			log.Printf("Cache full (%d MB), skipping local cache", *cacheSize)
			return
		}

		// Check if cache will overflow if file is added
		if cacheTotal+s.Size > *cacheSize*1024*1024 {
			log.Printf("File will overflow cache (%0.3f MB), skipping local cache", float64(s.Size)/1024/1024)
			return
		}

		// If file is greater than 50MB, skip caching to conserve memory
		threshold := 50
		if s.Size > int64(threshold*1024*1024) {
			log.Printf("File too large (%0.3f > %0d MB), skipping local cache", float64(s.Size)/1024/1024, threshold)
			return
		}

		// Generate a temporary file
		tmpFile, err := ioutil.TempFile(os.TempDir(), "subfs")
		if err != nil {
			log.Println(err)
			return
		}

		// Write out temporary file
		if _, err := tmpFile.Write(file); err != nil {
			log.Println(err)
			return
		}

		// Add file to cache map
		log.Printf("Caching file: [%d] %s", s.ID, s.FileName)
		fileCache[s.FileName] = *tmpFile

		// Add file's size to cache total size
		cacheTotal = atomic.AddInt64(&cacheTotal, s.Size)

		// Print some cache metrics
		cacheUse := float64(cacheTotal) / 1024 / 1024
		cacheAdd := float64(s.Size) / 1024 / 1024
		log.Printf("Cache use: %0.3f / %d.000 MB (+%0.3f MB)", cacheUse, *cacheSize, cacheAdd)

		return
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
