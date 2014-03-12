subfs
=====

Experimental FUSE filesystem for the [Subsonic](http://www.subsonic.org/pages/index.jsp) media server, written in Go.  MIT Licensed.

Full documentation for subfs may be found on [GoDoc](http://godoc.org/github.com/mdlayher/subfs).

Installation
============

subfs can be built using Go 1.1+. It can be downloaded, built, and installed, simply by running:

`$ go get github.com/mdlayher/subfs`

It should be noted that both subfs and its companion library, [gosubsonic](https://github.com/mdlayher/gosubsonic), are highly experimental.
These components are in need of much more testing, but I am happy with my progress thus far.

Usage
=====

To use subfs, simply run the binary and enter the appropriate command line flags to choose a mount point,
and to connect to your Subsonic media server:

`./subfs -host="demo.subsonic.org" -user="guest1" -password="guest" -mount="/tmp/subfs"`
