package service

import (
	"strings"

	xctx "golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/thecodeteam/gofsutil"
)

var (
	emptyMap = make(map[string]string, 0)
)

func (s *service) GetVolumeID(
	ctx xctx.Context,
	name string) (string, error) {

	if s.adminClient == nil {
		return "", status.Error(codes.FailedPrecondition,
			"Controller Service has not been probed")
	}

	id, err := s.adminClient.FindVolumeID(name)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayNotFound) {
			return "", nil
		} else {
			return "", err
		}
	}

	return id, nil
}

func (s *service) GetVolumeInfo(
	ctx xctx.Context,
	id, name string) (*csi.VolumeInfo, error) {

	if s.adminClient == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"Controller Service has not been probed")
	}

	if id == "" {
		if name == "" {
			return nil, status.Error(codes.Internal,
				"missing volume name and ID")
		}
		var err error
		id, err = s.GetVolumeID(ctx, name)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, nil
		}
	}

	vol, err := s.getVolByID(id)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			return nil, nil
		}
		return nil, err
	}

	info := getCSIVolumeInfo(vol)

	return info, nil
}

func (s *service) IsControllerPublished(
	ctx xctx.Context,
	id, nodeID string) (map[string]string, error) {

	if s.adminClient == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"Controller Service has not been probed")
	}

	vol, err := s.getVolByID(id)
	if err != nil {
		if strings.EqualFold(err.Error(), sioGatewayVolumeNotFound) {
			return nil, status.Error(codes.NotFound,
				"volume not found")
		}
		return nil, status.Errorf(codes.Internal,
			"failure checking volume status: %s",
			err.Error())
	}

	sdcID, err := s.getSDCID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, err.Error())
	}

	if len(vol.MappedSdcInfo) > 0 {
		for _, sdc := range vol.MappedSdcInfo {
			if sdc.SdcID == sdcID {
				return emptyMap, nil
			}
		}
	}

	return nil, nil
}

func (s *service) IsNodePublished(
	ctx xctx.Context,
	id string,
	pubInfo map[string]string,
	targetPath string) (bool, error) {

	sdcMappedVol, err := getMappedVol(id)
	if err != nil {
		return false, nil
	}

	sysDevice, err := GetDevice(sdcMappedVol.SdcDevice)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"error getting block device for volume: %s, err: %s",
			id, err.Error())
	}

	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return false, err
	}

	devMnts := make([]gofsutil.Info, 0)

	for _, m := range mnts {
		if m.Device == sysDevice.RealDev || (m.Device == "devtmpfs" && m.Source == sysDevice.RealDev) {
			devMnts = append(devMnts, m)
		}
	}

	if len(devMnts) > 0 {
		for _, m := range devMnts {
			if m.Path == targetPath {
				return true, nil
			}
		}
	}

	return false, nil
}
