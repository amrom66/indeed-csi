package pkg

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"indeed-csi/pkg/state"

	"github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	utilexec "k8s.io/utils/exec"
)

const (
	kib    int64 = 1024
	mib    int64 = kib * 1024
	gib    int64 = mib * 1024
	gib100 int64 = gib * 100
	tib    int64 = gib * 1024
	tib100 int64 = tib * 100

	storageKind = "kind"
)

type indeed struct {
	config Config

	mutex sync.Mutex
	state state.State
}

type Config struct {
	DriverName                 string
	Endpoint                   string
	ProxyEndpoint              string
	NodeID                     string
	VendorVersion              string
	StateDir                   string
	MaxVolumesPerNode          int64
	MaxVolumeSize              int64
	AttachLimit                int64
	Capacity                   Capacity
	Ephemeral                  bool
	ShowVersion                bool
	EnableAttach               bool
	EnableTopology             bool
	EnableVolumeExpansion      bool
	DisableControllerExpansion bool
	DisableNodeExpansion       bool
	MaxVolumeExpansionSizeNode int64
	CheckVolumeLifecycle       bool
}

var (
	vendorVersion = "dev"
)

const (
	// Extension with snap snapshot files will be saved.
	snapshotExt = ".snap"
)

func NewIndeedDriver(cfg Config) (*indeed, error) {
	if cfg.DriverName == "" {
		return nil, errors.New("no driver name provided")
	}

	if cfg.NodeID == "" {
		return nil, errors.New("no node id provided")
	}

	if cfg.Endpoint == "" {
		return nil, errors.New("no driver endpoint provided")
	}

	if err := os.MkdirAll(cfg.StateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create dataRoot: %v", err)
	}

	glog.Infof("Driver: %v ", cfg.DriverName)
	glog.Infof("Version: %s", cfg.VendorVersion)

	s, err := state.New(path.Join(cfg.StateDir, "state.json"))
	if err != nil {
		return nil, err
	}
	hp := &indeed{
		config: cfg,
		state:  s,
	}
	return hp, nil
}

func (ind *indeed) Run() error {
	s := NewNonBlockingGRPCServer()
	s.Start(ind.config.Endpoint, ind, ind, ind)
	s.Wait()

	return nil
}

// getVolumePath returns the canonical path for hostpath volume
func (ind *indeed) getVolumePath(volID string) string {
	return filepath.Join(ind.config.StateDir, volID)
}

// getSnapshotPath returns the full path to where the snapshot is stored
func (ind *indeed) getSnapshotPath(snapshotID string) string {
	return filepath.Join(ind.config.StateDir, fmt.Sprintf("%s%s", snapshotID, snapshotExt))
}

// createVolume allocates capacity, creates the directory for the hostpath volume, and
// adds the volume to the list.
//
// It returns the volume path or err if one occurs. That error is suitable as result of a gRPC call.
func (ind *indeed) createVolume(volID, name string, cap int64, volAccessType state.AccessType, ephemeral bool, kind string) (*state.Volume, error) {
	// Check for maximum available capacity
	if cap > ind.config.MaxVolumeSize {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", cap, ind.config.MaxVolumeSize)
	}
	if ind.config.Capacity.Enabled() {
		if kind == "" {
			// Pick some kind with sufficient remaining capacity.
			for k, c := range ind.config.Capacity {
				if ind.sumVolumeSizes(k)+cap <= c.Value() {
					kind = k
					break
				}
			}
		}
		if kind == "" {
			// Still nothing?!
			return nil, status.Errorf(codes.ResourceExhausted, "requested capacity %d of arbitrary storage exceeds all remaining capacity", cap)
		}
		used := ind.sumVolumeSizes(kind)
		available := ind.config.Capacity[kind]
		if used+cap > available.Value() {
			return nil, status.Errorf(codes.ResourceExhausted, "requested capacity %d exceeds remaining capacity for %q, %s out of %s already used",
				cap, kind, resource.NewQuantity(used, resource.BinarySI).String(), available.String())
		}
	} else if kind != "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("capacity tracking disabled, specifying kind %q is invalid", kind))
	}

	path := ind.getVolumePath(volID)

	switch volAccessType {
	case state.MountAccess:
		err := os.MkdirAll(path, 0777)
		if err != nil {
			return nil, err
		}
	case state.BlockAccess:
		executor := utilexec.New()
		size := fmt.Sprintf("%dM", cap/mib)
		// Create a block file.
		_, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				out, err := executor.Command("fallocate", "-l", size, path).CombinedOutput()
				if err != nil {
					return nil, fmt.Errorf("failed to create block device: %v, %v", err, string(out))
				}
			} else {
				return nil, fmt.Errorf("failed to stat block device: %v, %v", path, err)
			}
		}

		// Associate block file with the loop device.
		volPathHandler := volumepathhandler.VolumePathHandler{}
		_, err = volPathHandler.AttachFileDevice(path)
		if err != nil {
			// Remove the block file because it'll no longer be used again.
			if err2 := os.Remove(path); err2 != nil {
				glog.Errorf("failed to cleanup block file %s: %v", path, err2)
			}
			return nil, fmt.Errorf("failed to attach device %v: %v", path, err)
		}
	default:
		return nil, fmt.Errorf("unsupported access type %v", volAccessType)
	}

	volume := state.Volume{
		VolID:         volID,
		VolName:       name,
		VolSize:       cap,
		VolPath:       path,
		VolAccessType: volAccessType,
		Ephemeral:     ephemeral,
		Kind:          kind,
	}
	glog.V(4).Infof("adding hostpath volume: %s = %+v", volID, volume)
	if err := ind.state.UpdateVolume(volume); err != nil {
		return nil, err
	}
	return &volume, nil
}

// deleteVolume deletes the directory for the hostpath volume.
func (ind *indeed) deleteVolume(volID string) error {
	glog.V(4).Infof("starting to delete hostpath volume: %s", volID)

	vol, err := ind.state.GetVolumeByID(volID)
	if err != nil {
		// Return OK if the volume is not found.
		return nil
	}

	if vol.VolAccessType == state.BlockAccess {
		volPathHandler := volumepathhandler.VolumePathHandler{}
		path := ind.getVolumePath(volID)
		glog.V(4).Infof("deleting loop device for file %s if it exists", path)
		if err := volPathHandler.DetachFileDevice(path); err != nil {
			return fmt.Errorf("failed to remove loop device for file %s: %v", path, err)
		}
	}

	path := ind.getVolumePath(volID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := ind.state.DeleteVolume(volID); err != nil {
		return err
	}
	glog.V(4).Infof("deleted hostpath volume: %s = %+v", volID, vol)
	return nil
}

func (ind *indeed) sumVolumeSizes(kind string) (sum int64) {
	for _, volume := range ind.state.GetVolumes() {
		if volume.Kind == kind {
			sum += volume.VolSize
		}
	}
	return
}

// hostPathIsEmpty is a simple check to determine if the specified hostpath directory
// is empty or not.
func hostPathIsEmpty(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return true, fmt.Errorf("unable to open hostpath volume, error: %v", err)
	}
	defer f.Close()

	_, err = f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

// loadFromSnapshot populates the given destPath with data from the snapshotID
func (ind *indeed) loadFromSnapshot(size int64, snapshotId, destPath string, mode state.AccessType) error {
	snapshot, err := ind.state.GetSnapshotByID(snapshotId)
	if err != nil {
		return err
	}
	if !snapshot.ReadyToUse {
		return fmt.Errorf("snapshot %v is not yet ready to use", snapshotId)
	}
	if snapshot.SizeBytes > size {
		return status.Errorf(codes.InvalidArgument, "snapshot %v size %v is greater than requested volume size %v", snapshotId, snapshot.SizeBytes, size)
	}
	snapshotPath := snapshot.Path

	var cmd []string
	switch mode {
	case state.MountAccess:
		cmd = []string{"tar", "zxvf", snapshotPath, "-C", destPath}
	case state.BlockAccess:
		cmd = []string{"dd", "if=" + snapshotPath, "of=" + destPath}
	default:
		return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
	}

	executor := utilexec.New()
	glog.V(4).Infof("Command Start: %v", cmd)
	out, err := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
	glog.V(4).Infof("Command Finish: %v", string(out))
	if err != nil {
		return fmt.Errorf("failed pre-populate data from snapshot %v: %w: %s", snapshotId, err, out)
	}
	return nil
}

// loadFromVolume populates the given destPath with data from the srcVolumeID
func (ind *indeed) loadFromVolume(size int64, srcVolumeId, destPath string, mode state.AccessType) error {
	hostPathVolume, err := ind.state.GetVolumeByID(srcVolumeId)
	if err != nil {
		return err
	}
	if hostPathVolume.VolSize > size {
		return status.Errorf(codes.InvalidArgument, "volume %v size %v is greater than requested volume size %v", srcVolumeId, hostPathVolume.VolSize, size)
	}
	if mode != hostPathVolume.VolAccessType {
		return status.Errorf(codes.InvalidArgument, "volume %v mode is not compatible with requested mode", srcVolumeId)
	}

	switch mode {
	case state.MountAccess:
		return loadFromFilesystemVolume(hostPathVolume, destPath)
	case state.BlockAccess:
		return loadFromBlockVolume(hostPathVolume, destPath)
	default:
		return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
	}
}

func loadFromFilesystemVolume(hostPathVolume state.Volume, destPath string) error {
	srcPath := hostPathVolume.VolPath
	isEmpty, err := hostPathIsEmpty(srcPath)
	if err != nil {
		return fmt.Errorf("failed verification check of source hostpath volume %v: %w", hostPathVolume.VolID, err)
	}

	// If the source hostpath volume is empty it's a noop and we just move along, otherwise the cp call will fail with a a file stat error DNE
	if !isEmpty {
		args := []string{"-a", srcPath + "/.", destPath + "/"}
		executor := utilexec.New()
		out, err := executor.Command("cp", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed pre-populate data from volume %v: %s: %w", hostPathVolume.VolID, out, err)
		}
	}
	return nil
}

func loadFromBlockVolume(hostPathVolume state.Volume, destPath string) error {
	srcPath := hostPathVolume.VolPath
	args := []string{"if=" + srcPath, "of=" + destPath}
	executor := utilexec.New()
	out, err := executor.Command("dd", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed pre-populate data from volume %v: %w: %s", hostPathVolume.VolID, err, out)
	}
	return nil
}

func (ind *indeed) getAttachCount() int64 {
	count := int64(0)
	for _, vol := range ind.state.GetVolumes() {
		if vol.Attached {
			count++
		}
	}
	return count
}
