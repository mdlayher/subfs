package main

import (
	"bufio"
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

// Read opens a file stream from Subsonic, caches the stream as appropriate, and returns the
// resulting bytes needed with an offset and size applied
func (s SubFile) Read(req *fuse.ReadRequest, res *fuse.ReadResponse, intr fs.Intr) fuse.Error {
	// Byte stream to return data
	byteChan := make(chan []byte)

	// Fetch the file
	go s.fetchFile(req, byteChan)

	// Wait for an event on read
	select {
	// Byte stream channel
	case stream := <-byteChan:
		res.Data = stream
		//close(byteChan)
		return nil
	// Interrupt channel
	case <-intr:
		return fuse.EINTR
	}
}

// fetchFile invokes a file download request, and returns the subsequent cached stream
// for all other clients
func (s SubFile) fetchFile(req *fuse.ReadRequest, byteChan chan []byte) {
	// Check for file in cache
	if cFile, ok := fileCache[s.ID]; ok {
		// Make a buffer equal the requested size
		buf := make([]byte, req.Size)

		for {
			// Read the file at the specified offset into the buffer
			n, err := cFile.ReadAt(buf, req.Offset)

			// If bytes returned and no error or EOF detected, we got stream, so return it
			if err == nil || err == io.EOF {
				byteChan <- buf
				return
			} else if n == 0 && strings.Contains(err.Error(), "no such file or directory") {
				// File was removed from the cache, so purge it
				log.Printf("Cache missing: [%d] %s", s.ID, s.FileName)
				delete(fileCache, s.ID)
				cacheTotal = atomic.AddInt64(&cacheTotal, -1*s.Size)

				// Print some cache metrics
				cacheUse := float64(cacheTotal) / 1024 / 1024
				cacheDel := float64(s.Size) / 1024 / 1024
				log.Printf("Cache use: %0.3f / %d.000 MB (-%0.3f MB)", cacheUse, *cacheSize, cacheDel)

				// Close file handle
				if err := cFile.Close(); err != nil {
					log.Println(err)
				}

				// Break loop to begin re-opening stream
				break
			} else {
				// Some other condition occurred, so log it
				log.Println(err)
				<-time.After(1 * time.Second)
			}
		}
	}

	// Open stream
	stream, err := s.openStream()
	if err != nil {
		log.Println(err)
		byteChan <- nil
		return
	}

	// Generate a temporary file
	tmpFile, err := ioutil.TempFile(os.TempDir(), "subfs")
	if err != nil {
		log.Println(err)
		return
	}

	// Add file to cache map
	fileCache[s.ID] = *tmpFile

	// Invoke a recursive goroutine to wait for this file to be ready
	go s.fetchFile(req, byteChan)

	// Track total download size, for progress reporting
	var total int64
	atomic.StoreInt64(&total, 0)

	// Stop on file completion
	stopProgressChan := make(chan bool)
	go func() {
		// Print progress every second
		progress := time.NewTicker(1 * time.Second)

		// Calculate total file size
		totalSize := float64(s.Size)/1024/1024

		for {
			select {
			// Print progress
			case <-progress.C:
				// Capture current progress
				currTotal := atomic.LoadInt64(&total)
				current := float64(currTotal)/1024/1024

				// Capture current percentage
				percent := int64(float64(float64(total) / float64(s.Size)) * 100)

				log.Printf("[%d] [%03d%%] %0.3f / %0.3f MB", s.ID, percent, current, totalSize)
			// Stop printing
			case <-stopProgressChan:
				return
			}
		}
	}()

	// Read in the stream, dumping it to a temporary file as we go
	streamBuf := bufio.NewReader(stream)
	for {
		// Read one buffer from the stream
		buf := make([]byte, 8192)
		x, err := streamBuf.Read(buf)
		if x == 0 || err != nil {
			if err != io.EOF {
				log.Println(err)
			}

			// Store file size
			s.Size = atomic.LoadInt64(&total)

			break
		}

		atomic.AddInt64(&total, int64(x))

		// Write to the file
		y, err := tmpFile.Write(buf[:x])
		if y == 0 || err != nil {
			log.Println(err)
			break
		}
	}

	// Stop progress reporting
	stopProgressChan <- true

	// Close stream
	log.Printf("Closing stream: [%d] %s", s.ID, s.FileName)
	if err := stream.Close(); err != nil {
		log.Println(err)
		return
	}

	// Cache conditions
	// Check for maximum cache size
	cacheOne := cacheTotal > *cacheSize*1024*1024
	// Check if cache will overflow if file is added
	cacheTwo := cacheTotal+s.Size > *cacheSize*1024*1024
	// If file is greater than 50MB, skip caching to conserve memory
	threshold := 50
	cacheThree := s.Size > int64(threshold*1024*1024)

	// Print messages for failure conditions
	if cacheOne {
		log.Printf("Cache full (%d MB), skipping local cache", *cacheSize)
	} else if cacheTwo {
		log.Printf("File will overflow cache (%0.3f MB), skipping local cache", float64(s.Size)/1024/1024)
	} else if cacheThree {
		log.Printf("File too large (%0.3f > %0d MB), skipping local cache", float64(s.Size)/1024/1024, threshold)
	}

	// Check for ANY failure conditions, delete file if so
	if cacheOne || cacheTwo || cacheThree {
		// Close file
		if err := tmpFile.Close(); err != nil {
			log.Println(err)
		}

		// Remove file
		if err := os.Remove(tmpFile.Name()); err != nil {
			log.Println(err)
		}
		return
	}

	// Add file's size to cache total size
	cacheTotal = atomic.AddInt64(&cacheTotal, s.Size)

	// Print some cache metrics
	cacheUse := float64(cacheTotal) / 1024 / 1024
	cacheAdd := float64(s.Size) / 1024 / 1024
	log.Printf("Cache use: %0.3f / %d.000 MB (+%0.3f MB)", cacheUse, *cacheSize, cacheAdd)
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
		// Check if the Subsonic user is permitted to "download" raw files
		stream, err := subsonic.Download(s.ID)
		if strings.Contains(err.Error(), "not authorized to download files") {
			// Stream a transcoded file instead
			log.Printf("Opening transcoded audio stream: [%d] %s", s.ID, s.FileName)
			return subsonic.Stream(s.ID, nil)
		}

		// Attempt to get media file in raw, non-transcoded form
		log.Printf("Opening audio stream: [%d] %s", s.ID, s.FileName)
		return stream, nil
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
