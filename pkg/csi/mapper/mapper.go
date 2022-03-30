package mapper

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const mapperFilePath = "/var/lib/kubelet/local-ephemeral-mapper"

type VolumeDeviceMapper interface {
	Add(volumeID, device string) error
	Remove(volumeID string) error
	Get(volumeID string) (string, error)
	List() (map[string]string, error)
}

type volumeDeviceLocalMapper struct {
	// map is not thread safe. so "concurrent read/write of Map" can cause error.
	rwLock sync.RWMutex
	// mapper of volume id and device name
	volumeDeviceMap map[string]string
}

func NewVolumeDeviceLocalMapper() (VolumeDeviceMapper, error) {
	mapper, err := getMapperFromFile(mapperFilePath)
	if err != nil {
		return nil, err
	}
	return &volumeDeviceLocalMapper{
		volumeDeviceMap: mapper,
	}, nil
}

func (mapper *volumeDeviceLocalMapper) Add(volumeID, device string) error {
	mapper.rwLock.Lock()
	defer mapper.rwLock.Unlock()
	mapper.volumeDeviceMap[volumeID] = device
	if err := saveMapperToFile(mapper.volumeDeviceMap, mapperFilePath); err != nil {
		return fmt.Errorf("fail to save map to file %s: %s", mapperFilePath, err.Error())
	}
	return nil
}

func (mapper *volumeDeviceLocalMapper) Remove(volumeID string) error {
	mapper.rwLock.Lock()
	defer mapper.rwLock.Unlock()
	delete(mapper.volumeDeviceMap, volumeID)
	if err := saveMapperToFile(mapper.volumeDeviceMap, mapperFilePath); err != nil {
		return fmt.Errorf("fail to save map to file %s: %s", mapperFilePath, err.Error())
	}
	return nil
}

func (mapper *volumeDeviceLocalMapper) Get(volumeID string) (string, error) {
	mapper.rwLock.RLock()
	defer mapper.rwLock.RUnlock()
	return mapper.volumeDeviceMap[volumeID], nil
}

func (mapper *volumeDeviceLocalMapper) List() (map[string]string, error) {
	mapper.rwLock.RLock()
	defer mapper.rwLock.RUnlock()
	return mapper.volumeDeviceMap, nil
}

func getMapperFromFile(filePath string) (map[string]string, error) {
	var err error
	mapper := make(map[string]string)
	if _, err = os.Stat(filePath); os.IsNotExist(err) {
		return mapper, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fail to stat file %s: %s", filePath, err.Error())
	}

	var file *os.File
	file, err = os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("fail to open file %s: %s", filePath, err.Error())
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&mapper); err != nil {
		return nil, fmt.Errorf("fail to decode file %s: %s", filePath, err.Error())
	}

	return mapper, nil
}

func saveMapperToFile(mapper map[string]string, filePath string) error {
	var err error
	var file *os.File
	if file, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, os.ModeAppend); err != nil {
		return fmt.Errorf("fail to open file %s: %s", filePath, err.Error())
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(mapper); err != nil {
		return fmt.Errorf("fail to encode mapper to file %s: %s", filePath, err.Error())
	}
	return nil
}
