package encodedstorage

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"

	"github.com/minio-io/minio/pkgs/erasure"
	"github.com/minio-io/minio/pkgs/split"
	"github.com/minio-io/minio/pkgs/storage"
	"github.com/minio-io/minio/pkgs/storage/appendstorage"
)

type encodedStorage struct {
	RootDir     string
	K           int
	M           int
	BlockSize   uint64
	objects     map[string]StorageEntry
	diskStorage []storage.ObjectStorage
}

func NewStorage(rootDir string, k, m int, blockSize uint64) (storage.ObjectStorage, error) {
	// create storage files
	storageNodes := make([]storage.ObjectStorage, 16)
	for i := 0; i < 16; i++ {
		storageNode, err := appendstorage.NewStorage(rootDir, i)
		storageNodes[i] = storageNode
		if err != nil {
			return nil, err
		}
	}
	objects := make(map[string]StorageEntry)
	indexPath := path.Join(rootDir, "index")
	if _, err := os.Stat(indexPath); err == nil {
		indexFile, err := os.Open(indexPath)
		defer indexFile.Close()
		if err != nil {
			return nil, err
		}
		encoder := gob.NewDecoder(indexFile)
		err = encoder.Decode(&objects)
		if err != nil {
			return nil, err
		}
	}
	newStorage := encodedStorage{
		RootDir:     rootDir,
		K:           k,
		M:           m,
		BlockSize:   blockSize,
		objects:     objects,
		diskStorage: storageNodes,
	}
	return &newStorage, nil
}

func (eStorage *encodedStorage) Get(objectPath string) (io.Reader, error) {
	entry, ok := eStorage.objects[objectPath]
	if ok == false {
		return nil, nil
	}
	reader, writer := io.Pipe()
	go eStorage.readObject(objectPath, entry, writer)
	return reader, nil
}

func (eStorage *encodedStorage) List(listPath string) ([]storage.ObjectDescription, error) {
	return nil, errors.New("Not Implemented")
}

func (eStorage *encodedStorage) Put(objectPath string, object io.Reader) error {
	// split
	chunks := make(chan split.SplitMessage)
	go split.SplitStream(object, eStorage.BlockSize, chunks)

	// for each chunk
	encoderParameters, err := erasure.ParseEncoderParams(eStorage.K, eStorage.M, erasure.CAUCHY)
	if err != nil {
		return err
	}
	encoder := erasure.NewEncoder(encoderParameters)
	entry := StorageEntry{
		Path:   objectPath,
		Md5sum: "md5sum",
		Crc:    24,
		Blocks: make([]StorageBlockEntry, 0),
	}
	i := 0
	// encode
	for chunk := range chunks {
		if chunk.Err == nil {
			// encode each
			blocks, length := encoder.Encode(chunk.Data)
			// store each
			storeErrors := eStorage.storeBlocks(objectPath+"$"+strconv.Itoa(i), blocks)
			for _, err := range storeErrors {
				if err != nil {
					return err
				}
			}
			blockEntry := StorageBlockEntry{
				Index:  i,
				Length: length,
			}
			entry.Blocks = append(entry.Blocks, blockEntry)
		} else {
			return chunk.Err
		}
		i++
	}
	eStorage.objects[objectPath] = entry
	var gobBuffer bytes.Buffer
	gobEncoder := gob.NewEncoder(&gobBuffer)
	gobEncoder.Encode(eStorage.objects)
	ioutil.WriteFile(path.Join(eStorage.RootDir, "index"), gobBuffer.Bytes(), 0600)
	return nil
}

type storeRequest struct {
	path string
	data []byte
}

type storeResponse struct {
	data []byte
	err  error
}

type StorageEntry struct {
	Path   string
	Md5sum string
	Crc    uint32
	Blocks []StorageBlockEntry
}

type StorageBlockEntry struct {
	Index  int
	Length int
}

func (eStorage *encodedStorage) storeBlocks(path string, blocks [][]byte) []error {
	returnChannels := make([]<-chan error, len(eStorage.diskStorage))
	for i, store := range eStorage.diskStorage {
		returnChannels[i] = storageRoutine(store, path, bytes.NewBuffer(blocks[i]))
	}
	returnErrors := make([]error, 0)
	for _, returnChannel := range returnChannels {
		for returnValue := range returnChannel {
			if returnValue != nil {
				returnErrors = append(returnErrors, returnValue)
			}
		}
	}
	return returnErrors
}

func (eStorage *encodedStorage) readObject(objectPath string, entry StorageEntry, writer *io.PipeWriter) {
	params, err := erasure.ParseEncoderParams(eStorage.K, eStorage.M, erasure.CAUCHY)
	if err != nil {
	}
	encoder := erasure.NewEncoder(params)
	for i, chunk := range entry.Blocks {
		blockSlices := eStorage.getBlockSlices(objectPath + "$" + strconv.Itoa(i))
		var blocks [][]byte
		for _, slice := range blockSlices {
			if slice.err != nil {
				writer.CloseWithError(err)
				return
			}
			blocks = append(blocks, slice.data)
		}
		data, err := encoder.Decode(blocks, chunk.Length)
		if err != nil {
			writer.CloseWithError(err)
			return
		}
		bytesWritten := 0
		for bytesWritten != len(data) {
			written, err := writer.Write(data[bytesWritten:len(data)])
			if err != nil {
				writer.CloseWithError(err)
			}
			bytesWritten += written
		}
	}
	writer.Close()
}

func (eStorage *encodedStorage) getBlockSlices(objectPath string) []storeResponse {
	responses := make([]<-chan storeResponse, 0)
	for i := 0; i < len(eStorage.diskStorage); i++ {
		response := getSlice(eStorage.diskStorage[i], objectPath)
		responses = append(responses, response)
	}
	results := make([]storeResponse, 0)
	for _, response := range responses {
		results = append(results, <-response)
	}
	return results
}

func getSlice(store storage.ObjectStorage, path string) <-chan storeResponse {
	out := make(chan storeResponse)
	go func() {
		obj, err := store.Get(path)
		if err != nil {
			out <- storeResponse{data: nil, err: err}
		} else {
			data, err := ioutil.ReadAll(obj)
			out <- storeResponse{data: data, err: err}
		}
		close(out)
	}()
	return out
}

func storageRoutine(store storage.ObjectStorage, path string, data io.Reader) <-chan error {
	out := make(chan error)
	go func() {
		if err := store.Put(path, data); err != nil {
			out <- err
		}
		close(out)
	}()
	return out
}
