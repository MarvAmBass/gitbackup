package engine

// runIndex regenerates the index and landing pages when repos changed. The
// history/file/commit pages are rendered on demand by streagit.
func (e *Engine) runIndex() {
	if e.Pages == nil {
		return
	}
	e.mu.Lock()
	dirty := e.indexDirty
	e.indexDirty = false
	e.mu.Unlock()
	if !dirty {
		return
	}
	repos, err := e.Store.AllRepos()
	if err == nil {
		stars, _ := e.Store.StarViews()
		err = e.Pages.Index(repos, stars, e.Cfg.Get().Settings.HTML)
	}
	if err != nil {
		e.Log.Warn("html index", "err", err)
	}
}
