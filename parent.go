package tableroll

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	fdsock "github.com/ftrvxmtrx/fd"
	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
)

const (
	notifyReady = 42
)

type parent struct {
	wr          *net.UnixConn
	coordinator *coordinator
	l           log15.Logger
}

func pidIsDead(osi osIface, pid int) bool {
	proc, _ := osi.FindProcess(pid)
	return proc.Signal(syscall.Signal(0)) != nil
}

func newParent(l log15.Logger, osi osIface, coordinationDir string) (*coordinator, *parent, map[fileName]*file, error) {
	coord, err := lockCoordinationDir(osi, l, coordinationDir)
	if err != nil {
		return nil, nil, nil, err
	}

	// sock is used for all messages between two siblings
	sock, err := coord.ConnectParent()
	if err == errNoParent {
		return coord, nil, make(map[fileName]*file), nil
	}
	if err != nil {
		return coord, nil, nil, err
	}

	l.Info("connected to parent, getting fds")
	// First get the names of files to expect. This also lets us know how many FDs to get
	var names [][]string

	// Note: use length-prefixing and avoid decoding directly from the socket to
	// ensure the reader isn't put into buffered mode, at which point file
	// descriptors can get lost since go's io buffering is obviously not fd
	// aware.
	var jsonNameLength int32
	if err := binary.Read(sock, binary.BigEndian, &jsonNameLength); err != nil {
		return coord, nil, nil, fmt.Errorf("protocol error: could not read length of json: %v", err)
	}
	nameJSON := make([]byte, jsonNameLength)
	if n, err := io.ReadFull(sock, nameJSON); err != nil || n != int(jsonNameLength) {
		return coord, nil, nil, fmt.Errorf("unable to read expected name json length (expected %v, got (%v, %v))", jsonNameLength, n, err)
	}

	if err := json.Unmarshal(nameJSON, &names); err != nil {
		return coord, nil, nil, errors.Wrap(err, "can't decode names from parent process")
	}
	l.Debug("expecting files", "names", names)

	// Now grab all the FDs from the parent from the socket
	files := make(map[fileName]*file, len(names))
	sockFileNames := make([]string, 0, len(files))
	for _, parts := range names {
		// parts[2] is the 'addr', which is the best we've got for a filename.
		// TODO(euank): should we just use 'key.String()' like is used in newFile?
		// I want to check this by seeing what the 'filename' is on each end and if
		// it changes from the parent process to the next parent with how I have this.
		sockFileNames = append(sockFileNames, parts[2])
	}
	sockFiles, err := fdsock.Get(sock, len(sockFileNames), sockFileNames)
	if err != nil {
		return coord, nil, nil, err
	}
	if len(sockFiles) != len(names) {
		panic(fmt.Errorf("got %v sockfiles, but expected %v: %+v; %+v", len(sockFiles), len(names), sockFiles, names))
	}
	for i, parts := range names {
		var key fileName
		copy(key[:], parts)

		files[key] = &file{
			os.NewFile(sockFiles[i].Fd(), key.String()),
			sockFiles[i].Fd(),
		}
	}
	l.Info("got fds from old parent", "files", files)

	// now that we have the FDs from the old parent, we just need to tell it when we're ready and then we're done and happy!

	return coord, &parent{
		wr:          sock,
		coordinator: coord,
		l:           l,
	}, files, nil
}

func (ps *parent) sendReady() error {
	defer ps.wr.Close()
	if _, err := ps.wr.Write([]byte{notifyReady}); err != nil {
		return errors.Wrap(err, "can't notify parent process")
	}
	ps.l.Info("notified the parent process we're ready")
	// Now that we're ready and the old process is draining, take over and relinquish the lock.
	if err := ps.coordinator.BecomeParent(); err != nil {
		return err
	}
	if err := ps.coordinator.Unlock(); err != nil {
		return err
	}
	ps.l.Info("unlocked coordinator directory")
	return nil
}
