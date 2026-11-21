package rca

const (
	sessionBytes         = uint64(16 << 20)
	hostBytes            = uint64(128 << 20)
	storageMetadataBytes = uint64(16 << 10)
)

type volumeSpace struct {
	Total, Available uint64
	Identity         string
}

func spaceAllows(v volumeSpace, peak uint64) bool {
	if v.Identity == "" || v.Total == 0 || v.Available > v.Total {
		return false
	}
	threshold := v.Total / 100
	if v.Total%100 != 0 {
		threshold++
	}
	if threshold < 2<<30 {
		threshold = 2 << 30
	}
	return v.Available >= threshold && peak <= v.Available-threshold
}
