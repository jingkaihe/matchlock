package image

const (
	mb                      = int64(1024 * 1024)
	minLayerImageSizeMB     = int64(16)
	layerFixedHeadroomMB    = int64(8)
	layerHeadroomPercentDiv = int64(8) // ~12.5%
)

func estimateLayerImageSizeMB(totalSizeBytes int64) int64 {
	if totalSizeBytes < 0 {
		totalSizeBytes = 0
	}
	totalMB := (totalSizeBytes + mb - 1) / mb
	headroomMB := layerFixedHeadroomMB + (totalMB / layerHeadroomPercentDiv)
	sizeMB := totalMB + headroomMB
	if sizeMB < minLayerImageSizeMB {
		return minLayerImageSizeMB
	}
	return sizeMB
}
