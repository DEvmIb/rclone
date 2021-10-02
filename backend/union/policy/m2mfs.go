package policy

import (
	"context"
	"math"
	
	"github.com/rclone/rclone/backend/union/upstream"
	"github.com/rclone/rclone/fs"
)

func init() {
	registerPolicy("m2mfs", &M2Mfs{})
}

// Mfs stands for most free space
// Search category: same as epmfs.
// Action category: same as epmfs.
// Create category: Pick the drive with the most free space.
type M2Mfs struct {
	EpMfs
}

// Create category policy, governing the creation of files and directories
func (p *M2Mfs) Create(ctx context.Context, upstreams []*upstream.Fs, path string) ([]*upstream.Fs, error) {
	if len(upstreams) == 0 {
		return nil, fs.ErrorObjectNotFound
	}
	upstreams = filterNC(upstreams)
	if len(upstreams) == 0 {
		return nil, fs.ErrorPermissionDenied
	}

	var minUsedSpace int64 = math.MaxInt64
	var lusupstream *upstream.Fs

	for _, u := range upstreams {
		space, err := u.GetUsedSpace()
		if err != nil {
			fs.LogPrintf(fs.LogLevelNotice, nil,
				"Used Space is not supported for upstream %s, treating as 0", u.Name())
		}
		if space < minUsedSpace {
			minUsedSpace = space
			lusupstream = u
		}
	}



	u, err := p.mfs(upstreams)
	return []*upstream.Fs{u}, err
}