package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

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

// Create does nothing, because subfs is read-only
func (SubDir) Create(req *fuse.CreateRequest, res *fuse.CreateResponse, intr fs.Intr) (fs.Node, fs.Handle, fuse.Error) {
	return nil, nil, fuse.Errno(syscall.EROFS)
}

// Fsync does nothing, because subfs is read-only
func (SubDir) Fsync(req *fuse.FsyncRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Link does nothing, because subfs is read-only
func (SubDir) Link(req *fuse.LinkRequest, node fs.Node, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
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

// ReadDir returns a list of directory entries depending on the current path
func (d SubDir) ReadDir(intr fs.Intr) ([]fuse.Dirent, fuse.Error) {
	// List of directory entries to return
	directories := make([]fuse.Dirent, 0)

	// If at root of filesystem, fetch indexes
	if d.RelPath == "" {
		// If empty, wait for indexes to be available
		if len(indexCache) == 0 {
			<-indexChan
		}

		// Get index from cache
		index := indexCache

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
			// Automatically reject ID of 0
			if id == 0 {
				return false
			}

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

		// List of bad characters which should be replaced in filenames
		badChars := []string{"/", "\\"}

		// Iterate all returned directories
		for _, dir := range content.Directories {
			// Check for any characters which may cause trouble with filesystem display
			for _, b := range badChars {
				dir.Title = strings.Replace(dir.Title, b, "_", -1)
			}

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

		// Iterate all returned audio
		for _, a := range content.Audio {

			// Check for lossless and lossy transcode
			transcodes := []struct {
				suffix string
				size   int64
			}{
				{a.Suffix, a.Size},
				{a.TranscodedSuffix, 0},
			}

			for _, t := range transcodes {
				// If suffix is empty (source is lossy), skip this file
				if t.suffix == "" {
					continue
				}

				// Mark file as lossless by default
				lossless := true

				// If size is empty (transcode to lossy), estimate it and mark as lossy
				if t.size == 0 {
					lossless = false

					// Since we have no idea what Subsonic's transcoding settings are, we will estimate
					// using MP3 CBR 320 as our benchmark, being that it will likely over-estimate
					// Thanks: http://www.jeffreysward.com/editorials/mp3size.htm
					t.size = ((a.DurationRaw * 320) / 8) * 1024
				}

				// Predefined audio filename format
				audioFormat := fmt.Sprintf("%02d - %s - %s.%s", a.Track, a.Artist, a.Title, t.suffix)

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
					IsVideo:  false,
					Lossless: lossless,
					Size:     t.size,
				}

				// Check for cover art
				if unique(a.CoverArt, coverArt) {
					coverArt = append(coverArt, a.CoverArt)
				}

				// Append to list
				directories = append(directories, dir)
			}
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

// Mkdir does nothing, because subfs is read-only
func (SubDir) Mkdir(req *fuse.MkdirRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}

// Mknod does nothing, because subfs is read-only
func (SubDir) Mknod(req *fuse.MknodRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}

// Remove does nothing, because subfs is read-only
func (SubDir) Remove(req *fuse.RemoveRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Removexattr does nothing, because subfs is read-only
func (SubDir) Removexattr(req *fuse.RemovexattrRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Rename does nothing, because subfs is read-only
func (SubDir) Rename(req *fuse.RenameRequest, node fs.Node, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Setattr does nothing, because subfs is read-only
func (SubDir) Setattr(req *fuse.SetattrRequest, res *fuse.SetattrResponse, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Setxattr does nothing, because subfs is read-only
func (SubDir) Setxattr(req *fuse.SetxattrRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Symlink does nothing, because subfs is read-only
func (SubDir) Symlink(req *fuse.SymlinkRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}
