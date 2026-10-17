package store

import (
	"database/sql"
	"errors"
)

// Release is a stored release.
type Release struct {
	ID         int64
	RepoID     int64
	Tag        string
	ExtID      string
	Name       string
	Published  int64
	Prerelease bool
	MetaHash   string
	State      string
	FirstSeen  int64
	LastSeen   int64
	Assets     []Asset
}

// Asset is one stored (or skipped) release file version.
type Asset struct {
	ID        int64
	ReleaseID int64
	Name      string
	ExtID     string
	URL       string
	Size      int64
	Identity  string
	Digest    string
	SHA256    string
	Path      string
	State     string
	Note      string
	Updated   int64
}

// Releases returns all releases of a repo (newest first) with their assets.
func (s *Store) Releases(repoID int64) ([]Release, error) {
	rows, err := s.db.Query(`SELECT id,repo_id,tag,ext_id,name,published,prerelease,meta_hash,state,first_seen,last_seen
		FROM releases WHERE repo_id=? ORDER BY published DESC, id DESC`, repoID)
	if err != nil {
		return nil, err
	}
	var out []Release
	idx := map[int64]int{}
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Tag, &r.ExtID, &r.Name, &r.Published, &r.Prerelease, &r.MetaHash, &r.State,
			&r.FirstSeen, &r.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		idx[r.ID] = len(out)
		out = append(out, r)
	}
	rows.Close()
	arows, err := s.db.Query(`SELECT a.id,a.release_id,a.name,a.ext_id,a.url,a.size,a.identity,a.digest,a.sha256,a.path,a.state,a.note,a.updated
		FROM release_assets a JOIN releases r ON r.id=a.release_id WHERE r.repo_id=? ORDER BY a.name, a.id DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var a Asset
		if err := arows.Scan(&a.ID, &a.ReleaseID, &a.Name, &a.ExtID, &a.URL, &a.Size, &a.Identity, &a.Digest, &a.SHA256, &a.Path,
			&a.State, &a.Note, &a.Updated); err != nil {
			return nil, err
		}
		if i, ok := idx[a.ReleaseID]; ok {
			out[i].Assets = append(out[i].Assets, a)
		}
	}
	return out, arows.Err()
}

// SaveRelease inserts or updates a release and returns its id.
func (s *Store) SaveRelease(r Release) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM releases WHERE repo_id=? AND tag=?`, r.RepoID, r.Tag).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := s.db.Exec(`INSERT INTO releases(repo_id,tag,ext_id,name,published,prerelease,meta_hash,state,first_seen,last_seen)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, r.RepoID, r.Tag, r.ExtID, r.Name, r.Published, r.Prerelease, r.MetaHash, r.State, r.LastSeen, r.LastSeen)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	_, err = s.db.Exec(`UPDATE releases SET ext_id=?,name=?,published=?,prerelease=?,meta_hash=?,state=?,last_seen=? WHERE id=?`,
		r.ExtID, r.Name, r.Published, r.Prerelease, r.MetaHash, r.State, r.LastSeen, id)
	return id, err
}

func (s *Store) SetReleaseState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE releases SET state=? WHERE id=?`, state, id)
	return err
}

// SaveAsset inserts (ID==0) or updates an asset row.
func (s *Store) SaveAsset(a Asset) (int64, error) {
	if a.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO release_assets(release_id,name,ext_id,url,size,identity,digest,sha256,path,state,note,updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, a.ReleaseID, a.Name, a.ExtID, a.URL, a.Size, a.Identity, a.Digest, a.SHA256, a.Path, a.State, a.Note, now())
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := s.db.Exec(`UPDATE release_assets SET url=?,size=?,sha256=?,path=?,state=?,note=?,updated=? WHERE id=?`,
		a.URL, a.Size, a.SHA256, a.Path, a.State, a.Note, now(), a.ID)
	return a.ID, err
}

// ReleaseSummary is shown in repo lists.
type ReleaseSummary struct {
	Releases, Assets int
	Bytes            int64
}

func (s *Store) ReleaseSummary(repoID int64) ReleaseSummary {
	var rs ReleaseSummary
	s.db.QueryRow(`SELECT count(*) FROM releases WHERE repo_id=?`, repoID).Scan(&rs.Releases)
	s.db.QueryRow(`SELECT count(*), COALESCE(sum(a.size),0) FROM release_assets a JOIN releases r ON r.id=a.release_id
		WHERE r.repo_id=? AND a.state IN ('ok','replaced','deleted') AND a.path!=''`, repoID).Scan(&rs.Assets, &rs.Bytes)
	return rs
}
