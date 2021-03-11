package builder

import (
	"context"
	"sync"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	re_filesystem "github.com/buildbarn/bb-remote-execution/pkg/filesystem"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteworker"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/resourceusage"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/protobuf/types/known/anypb"
)

type filePoolStatsBuildExecutor struct {
	buildExecutor BuildExecutor
}

// NewFilePoolStatsBuildExecutor creates a decorator for BuildExecutor
// that annotates ExecuteResponses to contain usage statistics of the
// FilePool. FilePools are used to allocate temporary files that are
// generated by the build action (e.g., output files).
func NewFilePoolStatsBuildExecutor(buildExecutor BuildExecutor) BuildExecutor {
	return &filePoolStatsBuildExecutor{
		buildExecutor: buildExecutor,
	}
}

func (be *filePoolStatsBuildExecutor) Execute(ctx context.Context, filePool re_filesystem.FilePool, instanceName digest.InstanceName, request *remoteworker.DesiredState_Executing, executionStateUpdates chan<- *remoteworker.CurrentState_Executing) *remoteexecution.ExecuteResponse {
	fp := statsCollectingFilePool{base: filePool}
	response := be.buildExecutor.Execute(ctx, &fp, instanceName, request, executionStateUpdates)

	fp.lock.Lock()
	stats := fp.stats
	fp.lock.Unlock()

	if resourceUsage, err := anypb.New(&stats); err == nil {
		response.Result.ExecutionMetadata.AuxiliaryMetadata = append(response.Result.ExecutionMetadata.AuxiliaryMetadata, resourceUsage)
	} else {
		attachErrorToExecuteResponse(response, util.StatusWrap(err, "Failed to marshal file pool resource usage"))
	}
	return response
}

// statsCollectingFilePool is a decorator for FilePool that measures the
// number of files created and the number of operations performed.
type statsCollectingFilePool struct {
	base re_filesystem.FilePool

	lock       sync.Mutex
	stats      resourceusage.FilePoolResourceUsage
	totalSize  uint64
	totalFiles uint64
}

func (fp *statsCollectingFilePool) NewFile() (filesystem.FileReadWriter, error) {
	f, err := fp.base.NewFile()
	if err != nil {
		return nil, err
	}

	fp.lock.Lock()
	fp.stats.FilesCreated++
	fp.totalFiles++
	if fp.stats.FilesCountPeak < fp.totalFiles {
		fp.stats.FilesCountPeak = fp.totalFiles
	}
	fp.lock.Unlock()

	return &statsCollectingFileReadWriter{
		FileReadWriter: f,
		pool:           fp,
	}, nil
}

// statsCollectingFileReadWriter is a decorator for
// filesystem.FileReadWriter that measures the number of file operations
// performed.
type statsCollectingFileReadWriter struct {
	filesystem.FileReadWriter
	pool *statsCollectingFilePool

	size uint64
}

func (f *statsCollectingFileReadWriter) updateSizeLocked(newSize uint64) {
	fp := f.pool
	fp.totalSize -= f.size
	f.size = newSize
	fp.totalSize += f.size
	if fp.stats.FilesSizeBytesPeak < fp.totalSize {
		fp.stats.FilesSizeBytesPeak = fp.totalSize
	}
}

func (f *statsCollectingFileReadWriter) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.FileReadWriter.ReadAt(p, off)

	fp := f.pool
	fp.lock.Lock()
	fp.stats.ReadsCount++
	fp.stats.ReadsSizeBytes += uint64(n)
	fp.lock.Unlock()

	return n, err
}

func (f *statsCollectingFileReadWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.FileReadWriter.WriteAt(p, off)

	fp := f.pool
	fp.lock.Lock()
	fp.stats.WritesCount++
	fp.stats.WritesSizeBytes += uint64(n)
	if n > 0 {
		if newSize := uint64(off) + uint64(n); newSize > f.size {
			f.updateSizeLocked(newSize)
		}
	}
	fp.lock.Unlock()

	return n, err
}

func (f *statsCollectingFileReadWriter) Truncate(length int64) error {
	err := f.FileReadWriter.Truncate(length)

	fp := f.pool
	fp.lock.Lock()
	fp.stats.TruncatesCount++
	if err == nil {
		f.updateSizeLocked(uint64(length))
	}
	fp.lock.Unlock()

	return err
}

func (f *statsCollectingFileReadWriter) Close() error {
	err := f.FileReadWriter.Close()
	f.FileReadWriter = nil

	fp := f.pool
	fp.lock.Lock()
	fp.totalFiles--
	fp.totalSize -= f.size
	fp.lock.Unlock()
	f.pool = nil

	return err
}
