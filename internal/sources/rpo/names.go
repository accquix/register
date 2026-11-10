package rpo

import (
	"regexp"
	"sort"
	"time"
)

// Object is one file in the bucket.
type Object struct {
	Key  string
	Size int64
}

var (
	reDaily    = regexp.MustCompile(`^batch-daily/actual_(\d{4}-\d{2}-\d{2})\.json\.gz$`)
	rePart     = regexp.MustCompile(`^batch-init/init_(\d{4}-\d{2}-\d{2})_(\d{3})\.json\.gz$`)
	reManifest = regexp.MustCompile(`^batch-init/init_(\d{4}-\d{2}-\d{2})_list\.txt$`)
)

// Snapshot groups the files of one monthly snapshot.
type Snapshot struct {
	Date     time.Time
	Manifest *Object
	Parts    map[string]Object // file name (without prefix) → object
}

// Inventory is what the bucket currently holds.
type Inventory struct {
	Dailies   map[string]Object // "YYYY-MM-DD" → object
	Snapshots map[string]*Snapshot
}

// Classify sorts a bucket listing into daily files and snapshots. Unknown keys are ignored.
func Classify(objs []Object) Inventory {
	inv := Inventory{Dailies: map[string]Object{}, Snapshots: map[string]*Snapshot{}}
	snap := func(day string) *Snapshot {
		s := inv.Snapshots[day]
		if s == nil {
			d, _ := time.Parse("2006-01-02", day)
			s = &Snapshot{Date: d, Parts: map[string]Object{}}
			inv.Snapshots[day] = s
		}
		return s
	}
	for _, o := range objs {
		switch {
		case reDaily.MatchString(o.Key):
			inv.Dailies[reDaily.FindStringSubmatch(o.Key)[1]] = o
		case rePart.MatchString(o.Key):
			m := rePart.FindStringSubmatch(o.Key)
			snap(m[1]).Parts[o.Key[len("batch-init/"):]] = o
		case reManifest.MatchString(o.Key):
			obj := o
			snap(reManifest.FindStringSubmatch(o.Key)[1]).Manifest = &obj
		}
	}
	return inv
}

// KeyDate returns the date in the name of a daily file or a snapshot part.
func KeyDate(key string) (time.Time, bool) {
	for _, re := range []*regexp.Regexp{reDaily, rePart} {
		if m := re.FindStringSubmatch(key); m != nil {
			d, err := time.Parse("2006-01-02", m[1])
			return d, err == nil
		}
	}
	return time.Time{}, false
}

// SnapshotDates returns the snapshot dates, newest first.
func (inv Inventory) SnapshotDates() []string {
	out := make([]string, 0, len(inv.Snapshots))
	for d := range inv.Snapshots {
		out = append(out, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// OldestDaily returns the oldest daily date in the bucket, or "".
func (inv Inventory) OldestDaily() string {
	oldest := ""
	for d := range inv.Dailies {
		if oldest == "" || d < oldest {
			oldest = d
		}
	}
	return oldest
}

// NewestDaily returns the newest daily date in the bucket, or "".
func (inv Inventory) NewestDaily() string {
	newest := ""
	for d := range inv.Dailies {
		if d > newest {
			newest = d
		}
	}
	return newest
}
