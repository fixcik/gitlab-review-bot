package review

import (
	"context"
	"path/filepath"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/skills"
)

// Team skills (review.skills.dir, see internal/skills): for a review or a
// thread answer the skills are re-read from the directory, the ones the
// repository did not disable are staged as a read-only copy, the copy is
// mounted in the sandbox (RunRequest.SkillsDir) and the system prompt lists
// them. Afterwards the files Claude read tell which skills it consulted;
// they are written to the job log (/runs), never into GitLab notes.

// skillsRun is the skills of one Claude run. The zero value means "no
// skills" and all methods are safe on it.
type skillsRun struct {
	dir     string // staged copy on the host ("" = none)
	root    string // path Claude sees
	system  string // system prompt section
	names   []string
	cleanup func()
	reads   *jobctx.Reads
	log     interface{ Info(string, ...any) }
}

// prepareSkills stages the skills for a run. It never fails the run: any
// problem is logged and the run goes on without skills. The returned ctx
// collects the files Claude reads (see env.RunStructured).
func (r *Reviewer) prepareSkills(ctx context.Context, job core.Job, eff config.Effective) (*skillsRun, context.Context) {
	sr := &skillsRun{}
	if !eff.SkillsEnabled {
		return sr, ctx
	}
	log := r.env.Log.With("mr", job.MRKey())
	list, warns, err := skills.Load(r.env.Cfg.Review.Skills.Dir)
	if err != nil {
		log.Warn("skills: load failed, running without skills", "err", err)
		return sr, ctx
	}
	for _, w := range warns {
		log.Warn("skills: skipped", "detail", w)
	}
	list = skills.Select(list, eff.SkillsDisable)
	if len(list) == 0 {
		return sr, ctx
	}
	base, cleanup, err := r.learning.ScratchDir()
	if err != nil {
		log.Warn("skills: scratch dir", "err", err)
		return sr, ctx
	}
	dir := filepath.Join(base, "skills")
	if err := skills.Stage(dir, list); err != nil {
		_ = skills.Unstage(dir)
		cleanup()
		log.Warn("skills: stage failed, running without skills", "err", err)
		return sr, ctx
	}
	sr.dir = dir
	sr.root = core.SkillsMountPath(dir, r.env.Cfg.Sandbox.Mode == "docker")
	sr.system = skills.SystemSection(list, sr.root)
	for _, s := range list {
		sr.names = append(sr.names, s.Name)
	}
	sr.cleanup = func() { _ = skills.Unstage(dir); cleanup() }
	sr.reads, sr.log = &jobctx.Reads{}, log
	log.Info("skills: offered", "skills", sr.names)
	return sr, jobctx.WithReads(ctx, sr.reads)
}

// release removes the staged copy.
func (s *skillsRun) release() {
	if s != nil && s.cleanup != nil {
		if s.log != nil {
			s.log.Info("skills: used", "skills", s.used())
		}
		s.cleanup()
		s.cleanup = nil
	}
}

// used returns the skills Claude read during the run(s).
func (s *skillsRun) used() []string {
	if s == nil || s.reads == nil {
		return nil
	}
	return skills.Used(s.reads.Files(), s.root)
}
