package policy

import (
	"context"
	"sort"
	//"strconv"
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

	// we need more upstreams 
	if len(upstreams) < 2 {
		return upstreams, nil
	}

	type Disk struct {
		upstream	*upstream.Fs
		free   int64
	}

	var mirrors []*Disk
	var mirr []*upstream.Fs
	
	for _, u := range upstreams {
		space, _ := u.GetFreeSpace()
		var disk = new(Disk)
		disk.upstream=u
		disk.free=space
		mirrors=append(mirrors,disk)
	}

	sort.Slice(mirrors, func(i, j int) bool {
		return mirrors[i].free > mirrors[j].free
	})

	//for _, d := range mirrors {
	//	fs.LogPrintf(fs.LogLevelNotice, nil,"%s %s",strconv.FormatInt(d.free,10),d.upstream.Fs)
	//}
	
	mirr=append(mirr,mirrors[0].upstream)
	mirr=append(mirr,mirrors[1].upstream)

	if len(mirr) == 0 {
		return nil, fs.ErrorObjectNotFound
	}

	return mirr, nil
}
