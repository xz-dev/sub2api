package securityaudit

import (
	"context"
	"errors"
)

type Enqueuer struct {
	config  ConfigStore
	repo    JobRepository
	payload PayloadStore
	metrics Metrics
}

func NewEnqueuer(config ConfigStore, repo JobRepository, payload PayloadStore, metrics ...Metrics) *Enqueuer {
	var metric Metrics
	if len(metrics) > 0 {
		metric = metrics[0]
	}
	return &Enqueuer{config: config, repo: repo, payload: payload, metrics: metric}
}

// preparedJob is what an asynchronous audit keeps after the request: the
// snapshot, never the body. The snapshot is not small: its scan text is all
// the text the guard reads, up to the body's size. retained bounds the bytes
// it keeps alive; PromptService.Enqueue keeps that much of the request's
// memory charge until the commit goroutine drops the job.
type preparedJob struct {
	cfg        ActiveConfig
	snapshot   PromptSnapshot
	baseFields map[string]any
	retained   int64
}

// preparedJobOverhead bounds the job's fixed part: the snapshot's metadata
// strings and the log fields.
const preparedJobOverhead = 16 << 10

func (e *Enqueuer) Enqueue(ctx context.Context, req Request) error {
	job, err := e.prepare(req)
	if job == nil {
		return err
	}
	return e.commit(ctx, job)
}

// prepare runs the config checks and builds the snapshot from req.Body. It
// runs on the request's goroutine, so the body decode happens while the
// request still holds its memory charge; nil means the request is not
// audited (already logged), with an error only when the enqueuer is unusable.
func (e *Enqueuer) prepare(req Request) (*preparedJob, error) {
	if e == nil || e.config == nil || e.repo == nil || e.payload == nil {
		return nil, errors.New("prompt audit enqueuer unavailable")
	}
	cfg, ok := e.config.Active()
	baseFields := requestLogFields(req)
	if !ok || cfg.EffectiveMode() != ModeAsync {
		LogInfo(EventEnqueueSkipped, mergeLogFields(baseFields, map[string]any{"status": "skipped", "error_code": "mode_not_async"}))
		return nil, nil
	}
	baseFields["config_version"] = cfg.ConfigVersion
	if !cfg.IncludesGroup(req.GroupID) {
		LogInfo(EventEnqueueSkipped, mergeLogFields(baseFields, map[string]any{"status": "skipped", "error_code": "group_out_of_scope"}))
		return nil, nil
	}
	if len(cfg.EnabledEndpoints()) == 0 {
		e.recordDropped()
		LogWarn(EventEnqueueDropped, mergeLogFields(baseFields, map[string]any{"status": "dropped", "error_code": "no_enabled_endpoint"}))
		return nil, nil
	}
	snapshot, err := ExtractPromptSnapshot(req)
	if errors.Is(err, ErrNoPromptText) {
		LogInfo(EventEnqueueSkipped, mergeLogFields(baseFields, map[string]any{"status": "skipped", "error_code": "no_user_text"}))
		return nil, nil
	}
	if err != nil {
		e.recordDropped()
		LogWarn(EventEnqueueDropped, mergeLogFields(baseFields, map[string]any{"status": "dropped", "error_code": "snapshot_invalid"}))
		return nil, nil
	}
	// A scan text of one segment is that segment, a substring of the decoded
	// JSON string, which is at most the body; several segments are joined
	// into a scan text of exactly its length.
	text := int64(len(snapshot.ScanText))
	if snapshot.MessageCount <= 1 {
		text = max(text, int64(len(req.Body)))
	}
	retained := text + int64(len(snapshot.FullPrompt)+len(snapshot.RedactedPreview)) + preparedJobOverhead
	return &preparedJob{cfg: cfg, snapshot: snapshot, baseFields: baseFields, retained: retained}, nil
}

// commit stores a prepared job: staging row, payload, queue publish.
func (e *Enqueuer) commit(ctx context.Context, p *preparedJob) error {
	cfg, snapshot, baseFields := p.cfg, p.snapshot, p.baseFields
	job, err := e.repo.CreateStagingWithCapacity(ctx, snapshot.Redacted(), cfg.ConfigVersion, 3, cfg.QueueCapacity)
	if err != nil {
		code := "database_unavailable"
		if errors.Is(err, ErrQueueFull) {
			code = "queue_full"
		}
		if errors.Is(err, ErrQueueAdmissionBusy) {
			code = "queue_admission_busy"
		}
		LogWarn(EventEnqueueDropped, mergeLogFields(baseFields, map[string]any{
			"queue_capacity": cfg.QueueCapacity, "status": "dropped", "error_code": code,
		}))
		e.recordDropped()
		return err
	}
	if err := e.payload.Set(ctx, job.ID, snapshot.ScanText, DefaultPayloadTTL); err != nil {
		_ = e.repo.MarkStagingFailed(ctx, job.ID, "payload_store_failed", "payload store unavailable")
		LogWarn(EventEnqueueDropped, mergeLogFields(baseFields, map[string]any{
			"job_id": job.ID, "status": "dropped", "error_code": "payload_store_failed",
		}))
		e.recordDropped()
		return err
	}
	if err := e.repo.PublishQueued(ctx, job.ID); err != nil {
		_ = e.payload.Delete(ctx, job.ID)
		_ = e.repo.MarkStagingFailed(ctx, job.ID, "queue_publish_failed", "queue publish failed")
		LogWarn(EventEnqueueDropped, mergeLogFields(baseFields, map[string]any{
			"job_id": job.ID, "status": "dropped", "error_code": "queue_publish_failed",
		}))
		e.recordDropped()
		return err
	}
	LogInfo(EventJobEnqueued, mergeLogFields(baseFields, map[string]any{
		"job_id":         job.ID,
		"queue_capacity": cfg.QueueCapacity, "status": "queued",
	}))
	if e.metrics != nil {
		e.metrics.IncEnqueued()
	}
	return nil
}

func (e *Enqueuer) recordDropped() {
	if e != nil && e.metrics != nil {
		e.metrics.IncDropped()
	}
}
