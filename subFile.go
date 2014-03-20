package main

import (
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/mdlayher/gosubsonic"
)

// SubFile represents a file in Subsonic library
type SubFile struct {
	ID       int64
	Created  time.Time
	FileName string
	IsArt    bool
	IsVideo  bool
	Lossless bool
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
				return
			}
		}

		// Check for pre-existing stream in progress, so that multiple clients can receive it without
		// requesting the stream multiple times.  Yeah concurrency!
		if streamChan, ok := streamMap[s.ID]; ok {
			// Wait for stream to be ready, and return it
			byteChan <- <-streamChan
			return
		}

		// Generate a channel for clients wishing to wait on this stream
		streamMap[s.ID] = make(chan []byte, 0)

		// Open stream
		stream, err := s.openStream()
		if err != nil {
			log.Println(err)
			byteChan <- nil

			// Remove stream from map on error
			close(streamMap[s.ID])
			delete(streamMap, s.ID)
			return
		}

		// Read in stream
		file, err := ioutil.ReadAll(stream)
		if err != nil {
			log.Println(err)
			byteChan <- nil

			// Remove stream from map on error
			close(streamMap[s.ID])
			delete(streamMap, s.ID)
			return
		}

		// Calculate actual size upon retrieval
		s.Size = int64(len(file))

		// Close stream
		if err := stream.Close(); err != nil {
			log.Println(err)
			byteChan <- nil

			// Remove stream from map on error
			close(streamMap[s.ID])
			delete(streamMap, s.ID)
			return
		}

		// Return bytes
		log.Printf("Closing stream: [%d] %s", s.ID, s.FileName)
		byteChan <- file

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
		close(byteChan)
		return stream, nil
	// Interrupt channel
	case <-intr:
		return nil, fuse.EINTR
	}
}

// openStream returns the appropriate io.ReadCloser from a SubFile
func (s SubFile) openStream() (io.ReadCloser, error) {
	// Item is art
	if s.IsArt {
		log.Printf("Opening art stream: [%d] %s", s.ID, s.FileName)

		// Get cover art stream
		return subsonic.GetCoverArt(s.ID, -1)
	}

	// Else, item is audio or video

	// Check for lossless audio
	if !s.IsVideo && s.Lossless {
		// Attempt to get media file in raw, lossless form
		log.Printf("Opening lossless audio stream: [%d] %s", s.ID, s.FileName)
		return subsonic.Download(s.ID)
	}

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
		log.Printf("Opening transcoded audio stream: [%d] %s", s.ID, s.FileName)
	}

	// Get media file stream
	return subsonic.Stream(s.ID, &streamOptions)
}
