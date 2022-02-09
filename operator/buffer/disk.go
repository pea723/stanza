package buffer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-telemetry/opentelemetry-log-collection/entry"
	"github.com/open-telemetry/opentelemetry-log-collection/operator/helper"
	"go.uber.org/multierr"
	"golang.org/x/sync/semaphore"
)

var _ Buffer = (*DiskBuffer)(nil)

// DiskBufferConfig is a configuration struct for a DiskBuffer
type DiskBufferConfig struct {
	Type string `json:"type" yaml:"type"`

	// MaxSize is the maximum size in bytes of the data file on disk
	MaxSize helper.ByteSize `json:"max_size" yaml:"max_size"`

	// Path is a path to a directory which contains the data and metadata files
	Path string `json:"path" yaml:"path"`

	// Sync indicates whether to open the files with O_SYNC. If this is set to false,
	// in cases like power failures or unclean shutdowns, logs may be lost or the
	// database may become corrupted.
	Sync bool `json:"sync" yaml:"sync"`

	MaxChunkDelay helper.Duration `json:"max_delay"   yaml:"max_delay"`
	MaxChunkSize  uint            `json:"max_chunk_size" yaml:"max_chunk_size"`
}

// NewDiskBufferConfig creates a new default disk buffer config
func NewDiskBufferConfig() *DiskBufferConfig {
	return &DiskBufferConfig{
		Type:          "disk",
		MaxSize:       1 << 32, // 4GiB
		Sync:          true,
		MaxChunkDelay: helper.NewDuration(time.Second),
		MaxChunkSize:  1000,
	}
}

// Build creates a new Buffer from a DiskBufferConfig
func (c DiskBufferConfig) Build() (Buffer, error) {
	if c.Path == "" {
		return nil, os.ErrNotExist
	}

	bufferFilePath := filepath.Join(c.Path, "buffer")
	metadataFilePath := filepath.Join(c.Path, "metadata.json")

	metadata, err := OpenDiskBufferMetadata(metadataFilePath, c.Sync)
	if err != nil {
		return nil, err
	}

	cf, err := openCircularFile(bufferFilePath, c.Sync, int64(c.MaxSize))
	if err != nil {
		metadataCloseErr := metadata.Close()
		return nil, multierr.Combine(
			err,
			metadataCloseErr,
		)
	}

	cf.Start = metadata.StartOffset
	cf.ReadPtr = metadata.StartOffset
	cf.End = metadata.EndOffset
	cf.Full = metadata.Full

	sem := semaphore.NewWeighted(int64(c.MaxSize))
	acquired := sem.TryAcquire(cf.len())
	if !acquired {
		metadataCloseErr := metadata.Close()
		rbCloseErr := cf.Close()
		return nil, multierr.Combine(
			errors.New("failed to acquire buffer length for semaphore"),
			metadataCloseErr,
			rbCloseErr,
		)
	}

	return &DiskBuffer{
		metadata:      metadata,
		cf:            cf,
		cfMux:         &sync.Mutex{},
		writerSem:     sem,
		readerSem:     newGreedyCountingSemaphore(metadata.Entries),
		maxSize:       int64(c.MaxSize),
		maxChunkDelay: c.MaxChunkDelay.Duration,
		maxChunkSize:  c.MaxChunkSize,
		closedMux:     &sync.RWMutex{},
	}, nil
}

// DiskBuffer is a buffer of entries that stores the entries to disk.
// This buffer persists between application restarts.
type DiskBuffer struct {
	metadata *diskBufferMetadata
	// f is the underlying byte buffer for the disk buffer
	cf        *circularFile
	cfMux     *sync.Mutex
	writerSem *semaphore.Weighted
	readerSem *greedyCountingSemaphore
	// maxSize is the maximum number of entry bytes that can be written to the buffer file.
	maxSize int64
	// closed is a bool indicating if the buffer is closed
	closed    bool
	closedMux *sync.RWMutex

	maxChunkDelay time.Duration
	maxChunkSize  uint
}

// entryBufInitialSize is the initial size of the internal buffer that an entry
// is written to
const entryBufInitialSize = 0

// Add adds an entry onto the buffer.
// Will block if the buffer is full
func (d *DiskBuffer) Add(ctx context.Context, e *entry.Entry) error {
	d.closedMux.RLock()
	defer d.closedMux.RUnlock()

	if d.closed {
		return ErrBufferClosed
	}

	bufBytes := make([]byte, 0, entryBufInitialSize)
	bufBytes, err := marshalEntry(bufBytes, e)
	if err != nil {
		return err
	}

	if len(bufBytes) > int(d.maxSize) {
		return ErrEntryTooLarge
	}

	err = d.writerSem.Acquire(ctx, int64(len(bufBytes)))
	if err != nil {
		return err
	}

	d.cfMux.Lock()
	defer d.cfMux.Unlock()

	_, err = d.cf.Write(bufBytes)
	if err != nil {
		return err
	}

	d.metadata.EndOffset = d.cf.End
	d.metadata.Full = d.cf.Full
	d.metadata.Entries += 1
	err = d.metadata.Sync()
	if err != nil {
		return err
	}

	d.readerSem.Increment()

	return nil
}

// Read reads from the buffer.
// Read will block until the there are maxChunkSize entries or the duration maxChunkDelay has passed.
func (d *DiskBuffer) Read(ctx context.Context) ([]*entry.Entry, error) {
	d.closedMux.RLock()
	defer d.closedMux.RUnlock()

	if d.closed {
		return nil, ErrBufferClosed
	}

	n := d.readerSem.AcquireAtMost(ctx, d.maxChunkDelay, int64(d.maxChunkSize))

	if n == 0 {
		return nil, ctx.Err()
	}

	entries := make([]*entry.Entry, 0, n)

	d.cfMux.Lock()
	defer d.cfMux.Unlock()

	dec := json.NewDecoder(d.cf)

	for i := int64(0); i < n; i++ {
		var entry entry.Entry
		err := dec.Decode(&entry)
		if err != nil {
			return entries, err
		}

		entries = append(entries, &entry)
	}

	decoderOffset := dec.InputOffset()
	d.cf.discard(decoderOffset)
	d.writerSem.Release(decoderOffset)
	// Update start pointer to current position
	d.metadata.StartOffset = d.cf.Start
	d.metadata.Full = d.cf.Full
	d.metadata.Entries -= n
	err := d.metadata.Sync()

	return entries, err
}

// Close runs cleanup code for buffer.
func (d *DiskBuffer) Close() ([]*entry.Entry, error) {
	d.closedMux.Lock()
	defer d.closedMux.Unlock()

	if d.closed {
		return nil, nil
	}

	d.closed = true

	fileCloseErr := d.cf.Close()
	metaCloseErr := d.metadata.Close()
	return nil, multierr.Combine(
		fileCloseErr,
		metaCloseErr,
	)
}

// marshalEntry marshals the given entry into the given byte slice.
// It returns the buffer (which may be reallocated).
func marshalEntry(b []byte, e *entry.Entry) ([]byte, error) {
	buf := bytes.NewBuffer(b)
	enc := json.NewEncoder(buf)
	err := enc.Encode(e)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
