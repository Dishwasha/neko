package capture

import (
	"errors"
	"sync"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"m1k1o/neko/internal/capture/gst"
	"m1k1o/neko/internal/types"
	"m1k1o/neko/internal/types/codec"
)

type StreamSrcSinkManagerCtx struct {
	logger zerolog.Logger
	wg     sync.WaitGroup

	enabled       bool
	codecPipeline map[string]string // codec -> pipeline

	codec       codec.RTPCodec
	pipeline    *gst.Pipeline
	pipelineMu  sync.Mutex
	pipelineStr string

	sampleFn func(sample types.Sample)
}

func streamSrcSinkNew(enabled bool, codecPipeline map[string]string, video_id string) *StreamSrcSinkManagerCtx {
	logger := log.With().
		Str("module", "capture").
		Str("submodule", "stream-src-sink").
		Str("video_id", video_id).Logger()

	return &StreamSrcSinkManagerCtx{
		logger:        logger,
		enabled:       enabled,
		codecPipeline: codecPipeline,
	}
}

func (manager *StreamSrcSinkManagerCtx) shutdown() {
	manager.logger.Info().Msgf("shutdown")

	manager.Stop()
}

func (manager *StreamSrcSinkManagerCtx) OnSample(listener func(sample types.Sample)) {
	manager.sampleFn = listener
}

func (manager *StreamSrcSinkManagerCtx) Codec() codec.RTPCodec {
	manager.pipelineMu.Lock()
	defer manager.pipelineMu.Unlock()

	return manager.codec
}

func (manager *StreamSrcSinkManagerCtx) Start(codec codec.RTPCodec) error {
	manager.pipelineMu.Lock()
	defer manager.pipelineMu.Unlock()

	if manager.pipeline != nil {
		return types.ErrCapturePipelineAlreadyExists
	}

	if !manager.enabled {
		return errors.New("stream-src-sink not enabled")
	}

	found := false
	for codecName, pipeline := range manager.codecPipeline {
		if codecName == codec.Name {
			manager.pipelineStr = pipeline
			manager.codec = codec
			found = true
			break
		}
	}

	if !found {
		return errors.New("no pipeline found for a codec")
	}

	var err error

	manager.logger.Info().
		Str("codec", manager.codec.Name).
		Str("src", manager.pipelineStr).
		Msgf("creating pipeline")

	manager.pipeline, err = gst.CreatePipeline(manager.pipelineStr)
	if err != nil {
		return err
	}

	manager.pipeline.AttachAppsrc("appsrc")
	manager.pipeline.AttachAppsink("appsink")
	manager.pipeline.Play()

	manager.wg.Add(1)
	pipeline := manager.pipeline

	go func() {
		manager.logger.Debug().Msg("started emitting samples")
		defer manager.wg.Done()

		for {
			sample, ok := <-pipeline.Sample
			if !ok {
				manager.logger.Debug().Msg("stopped emitting samples")
				return
			}

			if manager.sampleFn != nil {
				manager.sampleFn(sample)
			}
		}
	}()

	return nil
}

func (manager *StreamSrcSinkManagerCtx) Stop() {
	manager.pipelineMu.Lock()
	defer manager.pipelineMu.Unlock()

	if manager.pipeline == nil {
		return
	}

	manager.pipeline.Destroy()
	manager.pipeline = nil

	manager.logger.Info().
		Str("codec", manager.codec.Name).
		Str("src", manager.pipelineStr).
		Msgf("destroying pipeline")
}

func (manager *StreamSrcSinkManagerCtx) Push(bytes []byte) {
	manager.pipelineMu.Lock()
	defer manager.pipelineMu.Unlock()

	if manager.pipeline == nil {
		return
	}

	manager.pipeline.Push(bytes)
}

func (manager *StreamSrcSinkManagerCtx) Started() bool {
	manager.pipelineMu.Lock()
	defer manager.pipelineMu.Unlock()

	return manager.pipeline != nil
}
