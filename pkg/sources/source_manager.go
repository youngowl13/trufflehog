package sources

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/sourcespb"
	"golang.org/x/sync/errgroup"
)

// handle uniquely identifies a Source given to the manager to manage. If the
// SourceManager is connected to the API, it will be equivalent to the unique
// source ID, otherwise it behaves as a counter.
type handle int64

// SourceInitFunc is a function that takes a source and job ID and returns an
// initialized Source.
type SourceInitFunc func(ctx context.Context, sourceID int64, jobID int64) (Source, error)

type SourceManager struct {
	api   apiClient
	hooks []JobProgressHook
	// Map of handle to source initializer.
	handles     map[handle]SourceInitFunc
	handlesLock sync.Mutex
	// Pool limiting the amount of concurrent sources running.
	pool            errgroup.Group
	concurrentUnits int
	// Run the sources using source unit enumeration / chunking if available.
	useSourceUnits bool
	// Downstream chunks channel to be scanned.
	outputChunks chan *Chunk
	// Set when Wait() returns.
	done bool
}

// apiClient is an interface for optionally communicating with an external API.
type apiClient interface {
	// RegisterSource lets the API know we have a source and it returns a unique source ID for it.
	RegisterSource(ctx context.Context, name string, kind sourcespb.SourceType) (int64, error)
	// GetJobID queries the API for an existing job or new job ID.
	GetJobID(ctx context.Context, id int64) (int64, error)
}

// WithAPI adds an API client to the manager for tracking jobs and progress. If
// the API is also a JobProgressHook, it will be added to the list of event hooks.
func WithAPI(api apiClient) func(*SourceManager) {
	return func(mgr *SourceManager) { mgr.api = api }
}

func WithReportHook(hook JobProgressHook) func(*SourceManager) {
	return func(mgr *SourceManager) {
		mgr.hooks = append(mgr.hooks, hook)
	}
}

// WithConcurrency limits the concurrent number of sources a manager can run.
func WithConcurrency(concurrency int) func(*SourceManager) {
	return func(mgr *SourceManager) { mgr.pool.SetLimit(concurrency) }
}

// WithBufferedOutput sets the size of the buffer used for the Chunks() channel.
func WithBufferedOutput(size int) func(*SourceManager) {
	return func(mgr *SourceManager) { mgr.outputChunks = make(chan *Chunk, size) }
}

// WithSourceUnits enables using source unit enumeration and chunking if the
// source supports it.
func WithSourceUnits() func(*SourceManager) {
	return func(mgr *SourceManager) { mgr.useSourceUnits = true }
}

// WithConcurrentUnits limits the number of units to be scanned concurrently.
// The default is unlimited.
func WithConcurrentUnits(n int) func(*SourceManager) {
	return func(mgr *SourceManager) { mgr.concurrentUnits = n }
}

// NewManager creates a new manager with the provided options.
func NewManager(opts ...func(*SourceManager)) *SourceManager {
	mgr := SourceManager{
		// Default to the headless API. Can be overwritten by the WithAPI option.
		api:          &headlessAPI{},
		handles:      make(map[handle]SourceInitFunc),
		outputChunks: make(chan *Chunk),
	}
	for _, opt := range opts {
		opt(&mgr)
	}
	return &mgr
}

// Enroll informs the SourceManager to track and manage a Source.
func (s *SourceManager) Enroll(ctx context.Context, name string, kind sourcespb.SourceType, f SourceInitFunc) (handle, error) {
	if s.done {
		return 0, fmt.Errorf("manager is done")
	}
	id, err := s.api.RegisterSource(ctx, name, kind)
	if err != nil {
		return 0, err
	}
	handleID := handle(id)
	s.handlesLock.Lock()
	defer s.handlesLock.Unlock()
	if _, ok := s.handles[handleID]; ok {
		// TODO: smartly handle this?
		return 0, fmt.Errorf("handle ID '%d' already in use", handleID)
	}
	s.handles[handleID] = f
	return handleID, nil
}

// Run blocks until a resource is available to run the source, then
// synchronously runs it. The first fatal error, if any, will be returned.
func (s *SourceManager) Run(ctx context.Context, handle handle) (JobProgressRef, error) {
	report, err := s.asyncRun(ctx, handle)
	if err != nil {
		return report, err
	}
	<-report.Done()
	return report, report.Snapshot().FatalError()
}

// ScheduleRun blocks until a resource is available to run the source, then
// asynchronously runs it. Error information is stored and accessible via the
// JobProgressRef as it becomes available.
func (s *SourceManager) ScheduleRun(ctx context.Context, handle handle) (JobProgressRef, error) {
	return s.asyncRun(ctx, handle)
}

// asyncRun is a helper method to asynchronously run the Source. It calls out
// to the API to get a job ID for this run, creates a report, then waits for an
// available goroutine to asynchronously run it.
func (s *SourceManager) asyncRun(ctx context.Context, handle handle) (JobProgressRef, error) {
	// Do preflight checks before waiting on the pool.
	if err := s.preflightChecks(ctx, handle); err != nil {
		return JobProgressRef{}, err
	}
	// Get a Job ID.
	jobID, err := s.api.GetJobID(ctx, int64(handle))
	if err != nil {
		return JobProgressRef{SourceID: int64(handle)}, err
	}
	// Start a report for this job.
	report := NewJobProgress(int64(handle), jobID, WithHooks(s.hooks...))
	s.pool.Go(func() error {
		defer common.Recover(ctx)
		_ = s.run(ctx, handle, jobID, report)
		return nil
	})
	return report.Ref(), nil
}

// Chunks returns the read only channel of all the chunks produced by all of
// the sources managed by this manager.
func (s *SourceManager) Chunks() <-chan *Chunk {
	return s.outputChunks
}

// Wait blocks until all running sources are completed and closes the channel
// returned by Chunks(). The manager should not be reused after calling this
// method. This current implementation is not thread safe and should only be
// called by one thread.
func (s *SourceManager) Wait() {
	// Check if the manager has been Waited.
	if s.done {
		return
	}
	defer close(s.outputChunks)
	defer func() { s.done = true }()

	// We are only using the errgroup for limiting concurrency.
	// TODO: Maybe switch to using a semaphore.Weighted.
	_ = s.pool.Wait()
}

// preflightChecks is a helper method to check the Manager or the context isn't
// done and that the handle is valid.
func (s *SourceManager) preflightChecks(ctx context.Context, handle handle) error {
	// Check if the manager has been Waited.
	if s.done {
		return fmt.Errorf("manager is done")
	}
	// Check the handle is valid.
	if _, ok := s.getInitFunc(handle); !ok {
		return fmt.Errorf("unrecognized handle")
	}
	return ctx.Err()
}

// run is a helper method to sychronously run the source. It does not check for
// acquired resources. An error is returned if there was a fatal error during
// the run. This information is also recorded in the JobProgress.
func (s *SourceManager) run(ctx context.Context, handle handle, jobID int64, report *JobProgress) error {
	defer report.Finish()
	report.Start(time.Now())
	defer func() { report.End(time.Now()) }()

	// Initialize the source.
	initFunc, ok := s.getInitFunc(handle)
	if !ok {
		// Shouldn't happen due to preflight checks.
		err := fmt.Errorf("unrecognized handle")
		report.ReportError(Fatal{err})
		return Fatal{err}
	}
	source, err := initFunc(ctx, int64(handle), jobID)
	if err != nil {
		report.ReportError(Fatal{err})
		return Fatal{err}
	}
	// Check for the preferred method of tracking source units.
	if enumChunker, ok := source.(SourceUnitEnumChunker); ok && s.useSourceUnits {
		return s.runWithUnits(ctx, handle, enumChunker, report)
	}
	return s.runWithoutUnits(ctx, handle, source, report)
}

// runWithoutUnits is a helper method to run a Source. It has coarse-grained
// job reporting.
func (s *SourceManager) runWithoutUnits(ctx context.Context, handle handle, source Source, report *JobProgress) error {
	// Introspect on the chunks we get from the Chunks method.
	ch := make(chan *Chunk)
	var wg sync.WaitGroup
	// Consume chunks and export chunks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for chunk := range ch {
			report.ReportChunk(nil, chunk)
			_ = common.CancellableWrite(ctx, s.outputChunks, chunk)
		}
	}()
	// Don't return from this function until the goroutine has finished
	// outputting chunks to the downstream channel. Closing the channel
	// will stop the goroutine, so that needs to happen first in the defer
	// stack.
	defer wg.Wait()
	defer close(ch)
	if err := source.Chunks(ctx, ch); err != nil {
		report.ReportError(Fatal{err})
		return Fatal{err}
	}
	return nil
}

// runWithUnits is a helper method to run a Source that is also a
// SourceUnitEnumChunker. This allows better introspection of what is getting
// scanned and any errors encountered.
func (s *SourceManager) runWithUnits(ctx context.Context, handle handle, source SourceUnitEnumChunker, report *JobProgress) error {
	unitReporter := &mgrUnitReporter{
		unitCh: make(chan SourceUnit),
		report: report,
	}
	// Create a function that will save the first error encountered (if
	// any) and discard the rest.
	fatalErr := make(chan error, 1)
	catchFirstFatal := func(err error) {
		select {
		case fatalErr <- err:
		default:
		}
	}
	// Produce units.
	go func() {
		// TODO: Catch panics and add to report.
		report.StartEnumerating(time.Now())
		defer func() { report.EndEnumerating(time.Now()) }()
		defer close(unitReporter.unitCh)
		if err := source.Enumerate(ctx, unitReporter); err != nil {
			report.ReportError(Fatal{err})
			catchFirstFatal(Fatal{err})
		}
	}()
	var wg sync.WaitGroup
	// TODO: Maybe switch to using a semaphore.Weighted.
	var unitPool errgroup.Group
	if s.concurrentUnits != 0 {
		// Negative values indicated no limit.
		unitPool.SetLimit(s.concurrentUnits)
	}
	for unit := range unitReporter.unitCh {
		unit := unit
		chunkReporter := &mgrChunkReporter{
			unit:    unit,
			chunkCh: make(chan *Chunk),
			report:  report,
		}
		// Consume units and produce chunks.
		unitPool.Go(func() error {
			report.StartUnitChunking(unit, time.Now())
			// TODO: Catch panics and add to report.
			defer close(chunkReporter.chunkCh)
			if err := source.ChunkUnit(ctx, unit, chunkReporter); err != nil {
				report.ReportError(Fatal{err})
				catchFirstFatal(Fatal{err})
			}
			return nil
		})
		// Consume chunks and export chunks.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { report.EndUnitChunking(unit, time.Now()) }()
			for chunk := range chunkReporter.chunkCh {
				report.ReportChunk(chunkReporter.unit, chunk)
				_ = common.CancellableWrite(ctx, s.outputChunks, chunk)
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-fatalErr:
		return err
	default:
		return nil
	}
}

// getInitFunc is a helper method for safe concurrent access to the
// map[handle]SourceInitFunc map.
func (s *SourceManager) getInitFunc(handle handle) (SourceInitFunc, bool) {
	s.handlesLock.Lock()
	defer s.handlesLock.Unlock()
	f, ok := s.handles[handle]
	return f, ok
}

// headlessAPI implements the apiClient interface locally.
type headlessAPI struct {
	// Counters for assigning handle and job IDs.
	sourceIDCounter int64
	jobIDCounter    int64
}

func (api *headlessAPI) RegisterSource(ctx context.Context, name string, kind sourcespb.SourceType) (int64, error) {
	return atomic.AddInt64(&api.sourceIDCounter, 1), nil
}

func (api *headlessAPI) GetJobID(ctx context.Context, id int64) (int64, error) {
	return atomic.AddInt64(&api.jobIDCounter, 1), nil
}

// mgrUnitReporter implements the UnitReporter interface.
type mgrUnitReporter struct {
	unitCh chan SourceUnit
	report *JobProgress
}

func (s *mgrUnitReporter) UnitOk(ctx context.Context, unit SourceUnit) error {
	return common.CancellableWrite(ctx, s.unitCh, unit)
}

func (s *mgrUnitReporter) UnitErr(ctx context.Context, err error) error {
	s.report.ReportError(err)
	return nil
}

// mgrChunkReporter implements the ChunkReporter interface.
type mgrChunkReporter struct {
	unit    SourceUnit
	chunkCh chan *Chunk
	report  *JobProgress
}

func (s *mgrChunkReporter) ChunkOk(ctx context.Context, chunk Chunk) error {
	return common.CancellableWrite(ctx, s.chunkCh, &chunk)
}

func (s *mgrChunkReporter) ChunkErr(ctx context.Context, err error) error {
	s.report.ReportError(ChunkError{s.unit, err})
	return nil
}
