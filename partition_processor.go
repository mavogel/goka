package goka

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/hashicorp/go-multierror"
	"github.com/lovoo/goka/multierr"
)

const (
	// PPStateIdle marks the partition processor as idling (not started yet)
	PPStateIdle State = iota
	// PPStateRecovering indicates a recovering partition processor
	PPStateRecovering
	// PPStateRunning indicates a running partition processor
	PPStateRunning
	// PPStateStopping indicates a stopping partition processor
	PPStateStopping
	// PPStateStopped indicates a stopped partition processor
	PPStateStopped
)

// PPRunMode configures how the partition processor participates as part of the processor
type PPRunMode int

const (
	// default mode: the processor recovers once and consumes messages
	runModeActive PPRunMode = iota
	// the processor keeps recovering. This is used for hot standby.
	runModePassive
	// the processor only recovers once and then stops. This is used for recover-ahead-option
	runModeRecoverOnly
)

type visit struct {
	key  string
	name string
	meta interface{}
	done func()
}

type commitCallback func(msg *message, meta string)

// PartitionProcessor handles message processing of one partition by serializing
// messages from different input topics.
// It also handles joined tables as well as lookup views (managed by `Processor`).
type PartitionProcessor struct {
	callbacks map[string]ProcessCallback

	log logger

	table   *PartitionTable
	joins   map[string]*PartitionTable
	lookups map[string]*View
	graph   *GroupGraph

	state *Signal

	partition int32

	input       chan *message
	inputTopics []string

	visitInput     chan *visit
	visitCallbacks map[string]ProcessCallback

	runnerGroup       *multierr.ErrGroup
	cancelRunnerGroup func()

	runMode PPRunMode

	consumer sarama.Consumer
	tmgr     TopicManager

	stats           *PartitionProcStats
	requestStats    chan bool
	responseStats   chan *PartitionProcStats
	updateStats     chan func()
	cancelStatsLoop context.CancelFunc

	commit   commitCallback
	producer Producer

	opts *poptions
}

func newPartitionProcessor(partition int32,
	graph *GroupGraph,
	commit commitCallback,
	logger logger,
	opts *poptions,
	runMode PPRunMode,
	lookupTables map[string]*View,
	consumer sarama.Consumer,
	producer Producer,
	tmgr TopicManager,
	backoff Backoff,
	backoffResetTime time.Duration) *PartitionProcessor {

	// collect all topics I am responsible for
	topicMap := make(map[string]bool)
	for _, stream := range graph.InputStreams() {
		topicMap[stream.Topic()] = true
	}
	if loop := graph.LoopStream(); loop != nil {
		topicMap[loop.Topic()] = true
	}

	var (
		topicList      []string
		outputList     []string
		callbacks      = make(map[string]ProcessCallback)
		visitCallbacks = make(map[string]ProcessCallback)
	)
	for t := range topicMap {
		topicList = append(topicList, t)
		callbacks[t] = graph.callback(t)
	}
	for _, output := range graph.OutputStreams() {
		outputList = append(outputList, output.Topic())
	}
	if graph.LoopStream() != nil {
		outputList = append(outputList, graph.LoopStream().Topic())
	}

	if graph.GroupTable() != nil {
		outputList = append(outputList, graph.GroupTable().Topic())
	}

	log := logger.Prefix(fmt.Sprintf("PartitionProcessor (%d)", partition))

	statsLoopCtx, cancel := context.WithCancel(context.Background())

	for _, v := range graph.visitors {
		visitCallbacks[v.(*visitor).name] = v.(*visitor).cb
	}

	partProc := &PartitionProcessor{
		log:             log,
		opts:            opts,
		partition:       partition,
		state:           NewSignal(PPStateIdle, PPStateRecovering, PPStateRunning, PPStateStopping, PPStateStopped).SetState(PPStateIdle),
		callbacks:       callbacks,
		lookups:         lookupTables,
		consumer:        consumer,
		producer:        producer,
		tmgr:            tmgr,
		joins:           make(map[string]*PartitionTable),
		input:           make(chan *message, opts.partitionChannelSize),
		inputTopics:     topicList,
		visitInput:      make(chan *visit, 100),
		visitCallbacks:  visitCallbacks,
		graph:           graph,
		stats:           newPartitionProcStats(topicList, outputList),
		requestStats:    make(chan bool),
		responseStats:   make(chan *PartitionProcStats, 1),
		updateStats:     make(chan func(), 10),
		cancelStatsLoop: cancel,
		commit:          commit,
		runMode:         runMode,
	}

	go partProc.runStatsLoop(statsLoopCtx)

	if graph.GroupTable() != nil {
		partProc.table = newPartitionTable(graph.GroupTable().Topic(),
			partition,
			consumer,
			tmgr,
			opts.updateCallback,
			opts.builders.storage,
			log.Prefix("PartTable"),
			backoff,
			backoffResetTime,
		)
	}
	return partProc
}

// Recovered returns whether the processor is running (i.e. all joins, lookups and the table is recovered and it's consuming messages)
func (pp *PartitionProcessor) Recovered() bool {
	return pp.state.IsState(PPStateRunning)
}

// Start initializes the partition processor
// * recover the table
// * recover all join tables
// * run the join-tables in catchup mode
// * start the processor processing loop to receive messages
// This method takes two contexts, as it does two distinct phases:
// * setting up the partition (loading table, joins etc.), after which it returns.
//   This needs a separate context to allow terminatin the setup phase
// * starting the message-processing-loop of the actual processor. This will keep running
//   after `Start` returns, so it uses the second context.
func (pp *PartitionProcessor) Start(setupCtx, ctx context.Context) error {

	if state := pp.state.State(); state != PPStateIdle {
		return fmt.Errorf("partitionprocessor is not idle (but %v), cannot start", state)
	}

	// runner context
	ctx, pp.cancelRunnerGroup = context.WithCancel(ctx)

	var runnerCtx context.Context
	pp.runnerGroup, runnerCtx = multierr.NewErrGroup(ctx)

	setupErrg, setupCtx := multierr.NewErrGroup(setupCtx)

	pp.state.SetState(PPStateRecovering)
	defer pp.state.SetState(PPStateRunning)

	if pp.table != nil {
		go pp.table.RunStatsLoop(runnerCtx)
		setupErrg.Go(func() error {
			pp.log.Debugf("catching up table")
			defer pp.log.Debugf("catching up table done")
			return pp.table.SetupAndRecover(setupCtx, false)
		})
	}

	for _, join := range pp.graph.JointTables() {
		table := newPartitionTable(join.Topic(),
			pp.partition,
			pp.consumer,
			pp.tmgr,
			pp.opts.updateCallback,
			pp.opts.builders.storage,
			pp.log.Prefix(fmt.Sprintf("Join %s", join.Topic())),
			NewSimpleBackoff(time.Second*10),
			time.Minute,
		)
		pp.joins[join.Topic()] = table

		go table.RunStatsLoop(runnerCtx)
		setupErrg.Go(func() error {
			return table.SetupAndRecover(setupCtx, false)
		})
	}

	// here we wait for our table and the joins to recover
	err := setupErrg.Wait().ErrorOrNil()
	if err != nil {
		return fmt.Errorf("Setup failed. Cannot start processor for partition %d: %v", pp.partition, err)
	}

	// check if one of the contexts might have been closed in the meantime
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	// at this point, we have successfully recovered all joins and the table of the partition-processor.
	// If the partition-processor was started to do only that (e.g. for group-recover-ahead), we
	// will return here
	if pp.runMode == runModeRecoverOnly {
		return nil
	}

	for _, join := range pp.joins {
		join := join
		pp.runnerGroup.Go(func() error {
			defer pp.state.SetState(PPStateStopping)
			return join.CatchupForever(runnerCtx, false)
		})
	}

	// now run the processor in a runner-group
	pp.runnerGroup.Go(func() error {
		defer pp.state.SetState(PPStateStopping)

		var err error
		// depending on the run mode, we'll do
		switch pp.runMode {
		// (a) start the processor's message run loop so it is ready to receive and process messages
		case runModeActive:
			err = pp.run(runnerCtx)
			// (b) run the processor table in catchup mode so it keeps updating it's state.
		case runModePassive:
			if pp.table != nil {
				err = pp.table.CatchupForever(runnerCtx, false)
			}
		default:
			err = fmt.Errorf("processor has invalid run mode")
		}
		if err != nil {
			pp.log.Debugf("Run failed with error: %v", err)
		}
		return err
	})
	return nil
}

func (pp *PartitionProcessor) stopping() <-chan struct{} {
	return pp.state.WaitForStateMin(PPStateStopping)
}

// Stop stops the partition processor
func (pp *PartitionProcessor) Stop() error {
	pp.log.Debugf("Stopping")
	defer pp.log.Debugf("... Stopping done")
	pp.state.SetState(PPStateStopping)
	defer pp.state.SetState(PPStateStopped)

	if pp.cancelRunnerGroup != nil {
		pp.cancelRunnerGroup()
	}

	// stop the stats updating/serving loop
	pp.cancelStatsLoop()

	// wait for the runner to be done
	runningErrs := multierror.Append(pp.runnerGroup.Wait().ErrorOrNil())

	// close all the tables
	stopErrg, _ := multierr.NewErrGroup(context.Background())
	for _, join := range pp.joins {
		join := join
		stopErrg.Go(func() error {
			return join.Close()
		})
	}

	// close processor table, if there is one
	if pp.table != nil {
		stopErrg.Go(func() error {
			return pp.table.Close()
		})
	}

	// return stopping errors and running errors
	return multierror.Append(stopErrg.Wait().ErrorOrNil(), runningErrs).ErrorOrNil()
}

func (pp *PartitionProcessor) run(ctx context.Context) (rerr error) {
	pp.log.Debugf("starting")
	defer pp.log.Debugf("stopped")

	// protect the errors-collection
	var mutexErr sync.Mutex

	defer func() {
		mutexErr.Lock()
		defer mutexErr.Unlock()
		rerr = multierror.Append(rerr).ErrorOrNil()
	}()

	var (
		// syncFailer is called synchronously from the callback within *this*
		// goroutine
		syncFailer = func(err error) {
			// only fail processor if context not already Done
			select {
			case <-ctx.Done():
				mutexErr.Lock()
				rerr = multierror.Append(rerr,
					newErrProcessing(pp.partition, fmt.Errorf("synchronous error in callback: %v", err)),
				)
				mutexErr.Unlock()
				return
			default:
			}
			panic(err)
		}

		closeOnce = new(sync.Once)
		asyncErrs = make(chan struct{})

		// asyncFailer is called asynchronously from other goroutines, e.g.
		// when the promise of an Emit (using a producer internally) fails
		asyncFailer = func(err error) {
			mutexErr.Lock()
			rerr = multierror.Append(rerr, newErrProcessing(pp.partition, fmt.Errorf("asynchronous error from callback: %v", err)))
			mutexErr.Unlock()
			closeOnce.Do(func() { close(asyncErrs) })
		}

		wg sync.WaitGroup
	)

	defer func() {
		if r := recover(); r != nil {
			mutexErr.Lock()
			rerr = multierror.Append(rerr,
				newErrProcessing(pp.partition, fmt.Errorf("panic in callback: %v\n%v", r, strings.Join(userStacktrace(), "\n"))),
			)
			mutexErr.Unlock()
			return
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.NewTimer(60 * time.Second).C:
			pp.log.Printf("partition processor did not shutdown in time. Will stop waiting")
		}
	}()

	for {
		select {
		case ev, isOpen := <-pp.input:
			// channel already closed, ev will be nil
			if !isOpen {
				return nil
			}
			err := pp.processMessage(ctx, &wg, ev, syncFailer, asyncFailer)
			if err != nil {
				return newErrProcessing(pp.partition, err)
			}

			pp.enqueueStatsUpdate(ctx, func() { pp.updateStatsWithMessage(ev) })
		case <-ctx.Done():
			pp.log.Debugf("exiting, context is cancelled")
			return
		case visit, open := <-pp.visitInput:
			if !open {
				return nil
			}
			err := pp.processVisit(ctx, &wg, visit, syncFailer, asyncFailer)
			if err != nil {
				return newErrProcessing(pp.partition, fmt.Errorf("Error visiting %s for %s: %v", visit.name, visit.key, err))
			}
		case <-asyncErrs:
			pp.log.Debugf("Errors occurred asynchronously. Will exit partition processor")
			return
		}
	}
}

func (pp *PartitionProcessor) enqueueStatsUpdate(ctx context.Context, updater func()) {
	select {
	case pp.updateStats <- updater:
	case <-ctx.Done():
	default:
		// going to default indicates the updateStats channel is not read, so so the stats
		// loop is not actually running.
		// We must not block here, so we'll skip the update
	}
}

func (pp *PartitionProcessor) runStatsLoop(ctx context.Context) {

	updateHwmStatsTicker := time.NewTicker(statsHwmUpdateInterval)
	defer updateHwmStatsTicker.Stop()
	for {
		select {
		case <-pp.requestStats:
			stats := pp.collectStats(ctx)
			select {
			case pp.responseStats <- stats:
			case <-ctx.Done():
				pp.log.Debugf("exiting, context is cancelled")
				return
			}
		case update := <-pp.updateStats:
			update()
		case <-updateHwmStatsTicker.C:
			pp.updateHwmStats()
		case <-ctx.Done():
			return
		}
	}
}

// updateStatsWithMessage updates the stats with a received message
func (pp *PartitionProcessor) updateStatsWithMessage(ev *message) {
	ip := pp.stats.Input[ev.topic]
	ip.Bytes += len(ev.value)
	ip.LastOffset = ev.offset
	if !ev.timestamp.IsZero() {
		ip.Delay = time.Since(ev.timestamp)
	}
	ip.Count++
}

// updateHwmStats updates the offset lag for all input topics based on the
// highwatermarks obtained by the consumer.
func (pp *PartitionProcessor) updateHwmStats() {
	hwms := pp.consumer.HighWaterMarks()
	for input, inputStats := range pp.stats.Input {
		hwm := hwms[input][pp.partition]
		if hwm != 0 && inputStats.LastOffset != 0 {
			inputStats.OffsetLag = hwm - inputStats.LastOffset
		}
	}
}

func (pp *PartitionProcessor) collectStats(ctx context.Context) *PartitionProcStats {
	var (
		stats = pp.stats.clone()
		m     sync.Mutex
	)

	errg, ctx := multierr.NewErrGroup(ctx)

	for topic, join := range pp.joins {
		topic, join := topic, join
		errg.Go(func() error {
			joinStats := join.fetchStats(ctx)
			if joinStats != nil {
				joinStats.RunMode = pp.runMode
			}
			m.Lock()
			defer m.Unlock()
			stats.Joined[topic] = joinStats
			return nil
		})
	}

	if pp.table != nil {
		errg.Go(func() error {
			stats.TableStats = pp.table.fetchStats(ctx)
			if stats.TableStats != nil {
				stats.TableStats.RunMode = pp.runMode
			}
			return nil
		})
	}

	err := errg.Wait().ErrorOrNil()
	if err != nil {
		pp.log.Printf("Error retrieving stats: %v", err)
	}

	return stats
}

func (pp *PartitionProcessor) fetchStats(ctx context.Context) *PartitionProcStats {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(fetchStatsTimeout):
		pp.log.Printf("requesting stats timed out")
		return nil
	case pp.requestStats <- true:
	}

	// retrieve from response-channel
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(fetchStatsTimeout):
		pp.log.Printf("Fetching stats timed out")
		return nil
	case stats := <-pp.responseStats:
		return stats
	}
}

func (pp *PartitionProcessor) enqueueTrackOutputStats(ctx context.Context, topic string, size int) {
	pp.enqueueStatsUpdate(ctx, func() {
		pp.stats.trackOutput(topic, size)
	})
}

func (pp *PartitionProcessor) processVisit(ctx context.Context, wg *sync.WaitGroup, v *visit, syncFailer func(err error), asyncFailer func(err error)) error {
	cb, ok := pp.visitCallbacks[v.name]
	// no callback registered for visit
	if !ok {
		return fmt.Errorf("no callback registered for visit named '%s'", v.name)
	}

	msgContext := &cbContext{
		ctx:   ctx,
		graph: pp.graph,

		trackOutputStats: pp.enqueueTrackOutputStats,
		pviews:           pp.joins,
		views:            pp.lookups,
		commit:           v.done,
		wg:               wg,
		msg: &message{
			key:       v.key,
			topic:     v.name,
			partition: pp.partition,
			timestamp: time.Now(),
		},
		syncFailer:            syncFailer,
		asyncFailer:           asyncFailer,
		emitter:               pp.producer.EmitWithHeaders,
		emitterDefaultHeaders: pp.opts.producerDefaultHeaders,
		table:                 pp.table,
	}

	// start context and call the ProcessorCallback cb
	msgContext.start()

	// now call cb
	cb(msgContext, v.meta)
	msgContext.finish(nil)
	return nil
}

func (pp *PartitionProcessor) processMessage(ctx context.Context, wg *sync.WaitGroup, msg *message, syncFailer func(err error), asyncFailer func(err error)) error {
	msgContext := &cbContext{
		ctx:   ctx,
		graph: pp.graph,

		trackOutputStats:      pp.enqueueTrackOutputStats,
		pviews:                pp.joins,
		views:                 pp.lookups,
		commit:                func() { pp.commit(msg, "") },
		wg:                    wg,
		msg:                   msg,
		syncFailer:            syncFailer,
		asyncFailer:           asyncFailer,
		emitter:               pp.producer.EmitWithHeaders,
		emitterDefaultHeaders: pp.opts.producerDefaultHeaders,
		table:                 pp.table,
	}

	var (
		m   interface{}
		err error
	)

	// decide whether to decode or ignore message
	switch {
	case msg.value == nil && pp.opts.nilHandling == NilIgnore:
		// mark the message upstream so we don't receive it again.
		// this is usually only an edge case in unit tests, as kafka probably never sends us nil messages
		pp.commit(msg, "")
		// otherwise drop it.
		return nil
	case msg.value == nil && pp.opts.nilHandling == NilProcess:
		// process nil messages without decoding them
		m = nil
	default:
		// get stream subcription
		codec := pp.graph.codec(msg.topic)
		if codec == nil {
			return fmt.Errorf("cannot handle topic %s", msg.topic)
		}

		// decode message
		m, err = codec.Decode(msg.value)
		if err != nil {
			return fmt.Errorf("error decoding message for key %s from %s/%d: %v", msg.key, msg.topic, msg.partition, err)
		}
	}

	cb := pp.callbacks[msg.topic]
	if cb == nil {
		return fmt.Errorf("error processing message for key %s from %s/%d: %v", msg.key, msg.topic, msg.partition, err)
	}

	// start context and call the ProcessorCallback cb
	msgContext.start()

	// now call cb
	cb(msgContext, m)
	msgContext.finish(nil)
	return nil
}

// VisitValues iterates over all values in the table and calls the "visit"-callback for the passed name.
// Optional parameter value can be set, which will just be forwarded to the visitor-function
func (pp *PartitionProcessor) VisitValues(ctx context.Context, name string, wg *sync.WaitGroup, meta interface{}) error {
	if pp.table == nil {
		return fmt.Errorf("cannot visit values in stateless processor")
	}
	if _, ok := pp.visitCallbacks[name]; !ok {
		return fmt.Errorf("unconfigured visit callback. Did you initialize the processor with DefineGroup(..., Visit(%s, ...), ...)?", name)
	}

	it, err := pp.table.Iterator()
	if err != nil {
		return fmt.Errorf("error creating storage iterator")
	}
	defer it.Release()
	for it.Next() {

		// add one that we're going to put into the queue
		// wg.Done will be called by the visit handler
		wg.Add(1)
		select {
		case <-ctx.Done():
			// context is closed, so we know that the current visit never entered the channel
			// so we have to remove it from the waitgroup to avoid the lock.
			wg.Done()
			return nil
		// enqueue the visit
		case pp.visitInput <- &visit{
			key:  string(it.Key()),
			name: name,
			meta: meta,
			done: wg.Done,
		}:
			// TODO (fe): add a notification when the processor shuts down so we can react to that instead of waiting for the global context
			// In this case we also have to drain the visitInput channel and call wg.Done() for each visit, so the caller does not wait for the waitgroup
		}
	}
	return nil
}
