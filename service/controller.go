package service

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi/v0"
	log "github.com/sirupsen/logrus"
	"github.com/thecodeteam/goscaleio"
	siotypes "github.com/thecodeteam/goscaleio/types/v1"
)

const (
	// KeyStoragePool is the key used to get the storagepool name from the
	// volume create parameters map
	KeyStoragePool = "storagepool"

	// DefaultVolumeSizeKiB is default volume size to create on a scaleIO
	// cluster when no size is given, expressed in KiB
	DefaultVolumeSizeKiB = 16 * kiBytesInGiB

	// VolSizeMultipleGiB is the volume size that ScaleIO creates volumes as
	// a multiple of, meaning that all volume sizes are a multiple of this
	// number
	VolSizeMultipleGiB = 8

	// bytesInKiB is the number of bytes in a kibibyte
	bytesInKiB = 1024

	// kiBytesInGiB is the number of kibibytes in a gibibyte
	kiBytesInGiB = 1024 * 1024

	// bytesInGiB is the number of bytes in a gibibyte
	bytesInGiB = kiBytesInGiB * bytesInKiB

	removeModeOnlyMe          = "ONLY_ME"
	sioGatewayNotFound        = "Not found"
	sioGatewayVolumeNotFound  = "Could not find the volume"
	sioGatewayVolumeNameInUse = "Volume name already in use. Please use a different name."
	errNoMultiMap             = "volume not enabled for mapping to multiple hosts"
	errUnknownAccessMode      = "access mode cannot be UNKNOWN"
	errNoMultiNodeWriter      = "multi-node with writer(s) only supported for block access type"
)

func (s *service) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	cr := req.GetCapacityRange()
	sizeInKiB, err := validateVolSize(cr)
	if err != nil {
		return nil, err
	}

	params := req.GetParameters()

	// We require the storagePool name for creation
	sp, ok := params[KeyStoragePool]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument,
			"`%s` is a required parameter", KeyStoragePool)
	}

	volType := s.getVolProvisionType(params)

	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument,
			"'name' cannot be empty")
	}

	// TODO handle Access mode in volume capability

	fields := map[string]interface{}{
		"name":        name,
		"sizeInKiB":   sizeInKiB,
		"storagePool": sp,
		"volType":     volType,
	}

	log.WithFields(fields).Info("creating volume")

	volumeParam := &siotypes.VolumeParam{
		Name:           name,
		VolumeSizeInKb: fmt.Sprintf("%d", sizeInKiB),
		VolumeType:     volType,
	}
	createResp, err := s.adminClient.CreateVolume(volumeParam, sp)
	if err != nil {
		// handle case where volume already exists
		if !strings.EqualFold(err.Error(), sioGatewayVolumeNameInUse) {
			return nil, status.Errorf(codes.Internal,
				"error when creating volume: %s", err.Error())
		}
	}

	var id string
	if createResp == nil {
		// volume already exists, look it up by name
		id, err = s.adminClient.FindVolumeID(name)
		if err != nil {
			return nil, status.Errorf(codes.Internal, err.Error())
		}
	} else {
		id = createResp.ID
	}

	vol, err := s.getVolByID(id)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"error retrieving volume details: %s", err.Error())
	}
	vi := getCSIVolume(vol)

	// since the volume could have already exists, double check that the
	// volume has the expected parameters
	spID, err := s.getStoragePoolID(sp)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"volume exists, but could not verify parameters: %s",
			err.Error())
	}
	if vol.StoragePoolID != spID {
		return nil, status.Errorf(codes.Unavailable,
			"volume exists, but in different storage pool than requested")
	}

	if (vi.CapacityBytes / bytesInKiB) != sizeInKiB {
		return nil, status.Errorf(codes.Unavailable,
			"volume exists, but at different size than requested")
	}

	csiResp := &csi.CreateVolumeResponse{
		Volume: vi,
	}

	s.clearCache()

	return csiResp, nil
}

func (s *service) clearCache() {
	s.volCacheRWL.Lock()
	defer s.volCacheRWL.Unlock()
	s.volCache = make([]*siotypes.Volume, 0)
}

// validateVolSize uses the CapacityRange range params to determine what size
// volume to create, and returns an error if volume size would be greater than
// the given limit. Returned size is in KiB
func validateVolSize(cr *csi.CapacityRange) (int64, error) {

	minSize := cr.GetRequiredBytes()
	maxSize := cr.GetLimitBytes()

	if minSize == 0 {
		minSize = DefaultVolumeSizeKiB
	} else {
		minSize = minSize / bytesInKiB
	}

	var (
		sizeGiB int64
		sizeKiB int64
		sizeB   int64
	)
	// ScaleIO creates volumes in multiples of 8GiB, rounding up.
	// Determine what actual size of volume will be, and check that
	// we do not exceed maxSize
	sizeGiB = minSize / kiBytesInGiB
	mod := sizeGiB % VolSizeMultipleGiB
	if mod > 0 {
		sizeGiB = sizeGiB - mod + VolSizeMultipleGiB
	}
	sizeB = sizeGiB * bytesInGiB
	if maxSize != 0 {
		if sizeB > maxSize {
			return 0, status.Errorf(
				codes.OutOfRange,
				"volume size %d > limit_bytes: %d", sizeB, maxSize)
		}
	}

	sizeKiB = sizeGiB * kiBytesInGiB
	return sizeKiB, nil
}

func (s *service) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	id := req.GetVolumeId()

	vol, err := s.getVolByID(id)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			log.Debug("volume already deleted")
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"failure checking volume status before deletion: %s",
			err.Error())
	}

	if len(vol.MappedSdcInfo) > 0 {
		// Volume is in use
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume in use by %s", vol.MappedSdcInfo[0].SdcID)
	}

	tgtVol := goscaleio.NewVolume(s.adminClient)
	tgtVol.Volume = vol
	err = tgtVol.RemoveVolume(removeModeOnlyMe)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"error removing volume: %s", err.Error())
	}

	s.clearCache()

	return &csi.DeleteVolumeResponse{}, nil
}

func (s *service) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument,
			"volumeID is required")
	}

	vol, err := s.getVolByID(volID)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			return nil, status.Error(codes.NotFound,
				"volume not found")
		}
		return nil, status.Errorf(codes.Internal,
			"failure checking volume status before controller publish: %s",
			err.Error())
	}

	nodeID := req.GetNodeId()
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument,
			"node ID is required")
	}

	sdcID, err := s.getSDCID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	vc := req.GetVolumeCapability()
	if vc == nil {
		return nil, status.Error(codes.InvalidArgument,
			"volume capability is required")
	}

	am := vc.GetAccessMode()
	if am == nil {
		return nil, status.Error(codes.InvalidArgument,
			"access mode is required")
	}

	if am.Mode == csi.VolumeCapability_AccessMode_UNKNOWN {
		return nil, status.Error(codes.InvalidArgument,
			errUnknownAccessMode)
	}
	// Check if volume is published to any node already
	if len(vol.MappedSdcInfo) > 0 {
		vcs := []*csi.VolumeCapability{req.GetVolumeCapability()}
		isBlock := accTypeIsBlock(vcs)

		for _, sdc := range vol.MappedSdcInfo {
			if sdc.SdcID == sdcID {
				// TODO check if published volume is compatible with this request
				// volume already mapped
				log.Debug("volume already mapped")
				return &csi.ControllerPublishVolumeResponse{}, nil
			}
		}

		// If volume has SINGLE_NODE cap, go no farther
		switch am.Mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume already published to SDC id: %s", vol.MappedSdcInfo[0].SdcID)
		}

		// All remaining cases are MULTI_NODE, make sure volume has
		// multi-map enabled
		if !vol.MappingToAllSdcsEnabled {
			return nil, status.Error(codes.FailedPrecondition,
				errNoMultiMap)
		}

		if err := validateAccessType(am, isBlock); err != nil {
			return nil, err
		}
	}

	mapVolumeSdcParam := &siotypes.MapVolumeSdcParam{
		SdcID: sdcID,
		AllowMultipleMappings: "false",
		AllSdcs:               "",
	}

	targetVolume := goscaleio.NewVolume(s.adminClient)
	targetVolume.Volume = &siotypes.Volume{ID: vol.ID}

	err = targetVolume.MapVolumeSdc(mapVolumeSdcParam)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"error mapping volume to node: %s", err.Error())
	}

	return &csi.ControllerPublishVolumeResponse{}, nil
}

func validateAccessType(
	am *csi.VolumeCapability_AccessMode,
	isBlock bool) error {

	if isBlock {
		switch am.Mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return nil
		default:
			return status.Errorf(codes.InvalidArgument,
				"Access mode: %v not compatible with access type", am.Mode)
		}
	} else {
		switch am.Mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			return nil
		default:
			return status.Errorf(codes.InvalidArgument,
				"Access mode: %v not compatible with access type", am.Mode)
		}
	}
}

func (s *service) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument,
			"volumeID is required")
	}

	vol, err := s.getVolByID(volID)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			return nil, status.Error(codes.NotFound,
				"volume not found")
		}
		return nil, status.Errorf(codes.Internal,
			"failure checking volume status before controller unpublish: %s",
			err.Error())
	}

	nodeID := req.GetNodeId()
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument,
			"Node ID is required")
	}

	sdcID, err := s.getSDCID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	// check if volume is attached to node at all
	mappedToNode := false
	for _, mapping := range vol.MappedSdcInfo {
		if mapping.SdcID == sdcID {
			mappedToNode = true
			break
		}
	}

	if !mappedToNode {
		log.Debug("volume already unpublished")
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	targetVolume := goscaleio.NewVolume(s.adminClient)
	targetVolume.Volume = vol

	unmapVolumeSdcParam := &siotypes.UnmapVolumeSdcParam{
		SdcID:                sdcID,
		IgnoreScsiInitiators: "true",
		AllSdcs:              "",
	}

	if err = targetVolume.UnmapVolumeSdc(unmapVolumeSdcParam); err != nil {
		return nil, status.Errorf(codes.Internal,
			"error unmapping volume from node: %s", err.Error())
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *service) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	volID := req.GetVolumeId()
	vol, err := s.getVolByID(volID)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			return nil, status.Error(codes.NotFound,
				"volume not found")
		}
		return nil, status.Errorf(codes.Internal,
			"failure checking volume status for capabilities: %s",
			err.Error())
	}

	vcs := req.GetVolumeCapabilities()
	supported, reason := valVolumeCaps(vcs, vol)

	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Supported: supported,
	}
	if !supported {
		resp.Message = reason
	}

	return resp, nil
}

func accTypeIsBlock(vcs []*csi.VolumeCapability) bool {
	for _, vc := range vcs {
		if at := vc.GetBlock(); at != nil {
			return true
		}
	}
	return false
}

func valVolumeCaps(
	vcs []*csi.VolumeCapability,
	vol *siotypes.Volume) (bool, string) {

	var (
		supported = true
		isBlock   = accTypeIsBlock(vcs)
		reason    string
	)

	for _, vc := range vcs {
		am := vc.GetAccessMode()
		if am == nil {
			continue
		}
		switch am.Mode {
		case csi.VolumeCapability_AccessMode_UNKNOWN:
			supported = false
			reason = errUnknownAccessMode
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
			fallthrough
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
			break
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			if !vol.MappingToAllSdcsEnabled {
				supported = false
				reason = errNoMultiMap
			}
		case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
			fallthrough
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			if !vol.MappingToAllSdcsEnabled {
				supported = false
				reason = errNoMultiMap
			}
			if !isBlock {
				supported = false
				reason = errNoMultiNodeWriter
			}
		}
	}

	return supported, reason
}

func (s *service) ListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	var (
		startToken int
		cacheLen   int
	)

	if v := req.StartingToken; v != "" {
		i, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, status.Errorf(
				codes.Aborted,
				"unable to parse startingToken:%v into uint32",
				req.StartingToken)
		}
		startToken = int(i)
	}

	// Get the length of cached volumes. Do it in a funcion so as not to
	// hold the lock
	func() {
		s.volCacheRWL.RLock()
		defer s.volCacheRWL.RUnlock()
		cacheLen = len(s.volCache)
	}()

	var (
		lvols      int
		sioVols    []*siotypes.Volume
		err        error
		maxEntries = int(req.MaxEntries)
	)

	if startToken == 0 || (startToken > 0 && cacheLen == 0) {
		// make call to cluster to get all volumes
		sioVols, err = s.adminClient.GetVolume("", "", "", "", false)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal,
				"unable to list volumes: %s", err.Error())
		}

		lvols = len(sioVols)
		if maxEntries > 0 && maxEntries < lvols {
			// We want to cache this volume list so that we don't
			// have to get all the volumes again on the next call
			func() {
				s.volCacheRWL.Lock()
				defer s.volCacheRWL.Unlock()
				s.volCache = make([]*siotypes.Volume, lvols)
				copy(s.volCache, sioVols)
				cacheLen = lvols
			}()
		}
	} else {
		lvols = cacheLen
	}

	if startToken > lvols {
		return nil, status.Errorf(
			codes.Aborted,
			"startingToken=%d > len(vols)=%d",
			startToken, lvols)
	}

	// Discern the number of remaining entries.
	rem := lvols - startToken

	// If maxEntries is 0 or greater than the number of remaining entries then
	// set maxEntries to the number of remaining entries.
	if maxEntries == 0 || maxEntries > rem {
		maxEntries = rem
	}

	var (
		entries = make(
			[]*csi.ListVolumesResponse_Entry,
			maxEntries)
		source []*siotypes.Volume
	)

	if startToken == 0 && req.MaxEntries == 0 {
		// Use the just populated sioVols
		source = sioVols
	} else {
		// Return only the requested vols from the cache
		cacheVols := make([]*siotypes.Volume, maxEntries)
		// Copy vols from cache so we don't keep lock entire time
		func() {
			s.volCacheRWL.RLock()
			defer s.volCacheRWL.RUnlock()
			j := startToken
			for i := 0; i < len(entries); i++ {
				cacheVols[i] = s.volCache[i]
				j++
			}
		}()
		source = cacheVols
	}

	for i, vol := range source {
		entries[i] = &csi.ListVolumesResponse_Entry{
			Volume: getCSIVolume(vol),
		}
	}

	var nextToken string
	if n := startToken + len(source); n < lvols {
		nextToken = fmt.Sprintf("%d", n)
	}

	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (s *service) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {

	if err := s.requireProbe(ctx); err != nil {
		return nil, err
	}

	var statsFunc func() (*siotypes.Statistics, error)

	// Default to get Capacity of system
	statsFunc = s.system.GetStatistics

	params := req.GetParameters()
	if len(params) > 0 {
		// if storage pool is given, get capacity of storage pool
		if spname, ok := params[KeyStoragePool]; ok {
			sp, err := s.adminClient.FindStoragePool("", spname, "")
			if err != nil {
				return nil, status.Errorf(codes.Internal,
					"unable to look up storage pool: %s, err: %s",
					spname, err.Error())
			}
			spc := goscaleio.NewStoragePoolEx(s.adminClient, sp)
			statsFunc = spc.GetStatistics
		}
	}
	stats, err := statsFunc()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to get system stats: %s", err.Error())
	}
	return &csi.GetCapacityResponse{
		AvailableCapacity: int64(stats.CapacityAvailableForVolumeAllocationInKb * bytesInKiB),
	}, nil
}

func (s *service) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
					},
				},
			},
			&csi.ControllerServiceCapability{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_GET_CAPACITY,
					},
				},
			},
		},
	}, nil
}

func (s *service) controllerProbe(ctx context.Context) error {

	// Check that we have the details needed to login to the Gateway
	if s.opts.Endpoint == "" {
		return status.Error(codes.FailedPrecondition,
			"missing ScaleIO Gateway endpoint")
	}
	if s.opts.User == "" {
		return status.Error(codes.FailedPrecondition,
			"missing ScaleIO MDM user")
	}
	if s.opts.Password == "" {
		return status.Error(codes.FailedPrecondition,
			"missing ScaleIO MDM password")
	}
	if s.opts.SystemName == "" {
		return status.Error(codes.FailedPrecondition,
			"missing ScaleIO system name")
	}

	// Create our ScaleIO API client, if needed
	if s.adminClient == nil {
		c, err := goscaleio.NewClientWithArgs(
			s.opts.Endpoint, "", s.opts.Insecure, true)
		if err != nil {
			return status.Errorf(codes.FailedPrecondition,
				"unable to create ScaleIO client: %s", err.Error())
		}
		s.adminClient = c
	}

	if s.adminClient.GetToken() == "" {
		_, err := s.adminClient.Authenticate(&goscaleio.ConfigConnect{
			Endpoint: s.opts.Endpoint,
			Username: s.opts.User,
			Password: s.opts.Password,
		})
		if err != nil {
			return status.Errorf(codes.FailedPrecondition,
				"unable to login to ScaleIO Gateway: %s", err.Error())

		}
	}

	if s.system == nil {
		system, err := s.adminClient.FindSystem(
			"", s.opts.SystemName, "")
		if err != nil {
			return status.Errorf(codes.FailedPrecondition,
				"unable to find matching ScaleIO system name: %s",
				err.Error())
		}
		s.system = system
	}

	return nil
}

func (s *service) requireProbe(ctx context.Context) error {
	if s.adminClient == nil {
		if !s.opts.AutoProbe {
			return status.Error(codes.FailedPrecondition,
				"Controller Service has not been probed")
		}
		log.Debug("probing controller service automatically")
		if err := s.controllerProbe(ctx); err != nil {
			return status.Errorf(codes.FailedPrecondition,
				"failed to probe/init plugin: %s", err.Error())
		}
	}
	return nil
}

func (s *service) CreateSnapshot(
        ctx context.Context,
        req *csi.CreateSnapshotRequest) (
        *csi.CreateSnapshotResponse, error) {

        return nil, status.Error(codes.Unimplemented, "")
}

func (s *service) DeleteSnapshot(
        ctx context.Context,
        req *csi.DeleteSnapshotRequest) (
        *csi.DeleteSnapshotResponse, error) {

        return nil, status.Error(codes.Unimplemented, "")
}

func (s *service) ListSnapshots(
        ctx context.Context,
        req *csi.ListSnapshotsRequest) (
        *csi.ListSnapshotsResponse, error) {

        return nil, status.Error(codes.Unimplemented, "")
}
