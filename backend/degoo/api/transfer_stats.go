package api

import (
	"io"
	"sync"
	"time"
)

const (
	transferStatsWindow      = 5 * time.Second
	transferStatsBucket      = 100 * time.Millisecond
	transferStatsBucketCount = int(transferStatsWindow/transferStatsBucket) + 1
)

// TransferStats reports object payload bytes handed to or read from storage
// HTTP bodies. Uploads count failed attempts and caller retries, but completion
// does not prove a remote acknowledgement or durable storage. Downloads include
// range and verification reads. Rates use retained bytes over a rolling window
// capped at five seconds.
type TransferStats struct {
	UploadBytes            int64   `json:"upload_bytes"`              // UploadBytes counts payload bytes read into GCS upload requests.
	UploadBytesPerSecond   float64 `json:"upload_bytes_per_second"`   // UploadBytesPerSecond is the recent upload rate.
	ActiveUploads          int     `json:"active_uploads"`            // ActiveUploads counts GCS upload requests in progress.
	DownloadBytes          int64   `json:"download_bytes"`            // DownloadBytes counts payload bytes read from storage responses.
	DownloadBytesPerSecond float64 `json:"download_bytes_per_second"` // DownloadBytesPerSecond is the recent download rate.
	ActiveDownloads        int     `json:"active_downloads"`          // ActiveDownloads counts download bodies not consumed, failed, or closed.
}

type transferBucket struct {
	period int64
	bytes  int64
}

// transferWindow tracks cumulative bytes and a bounded rolling sample window.
type transferWindow struct {
	mu         sync.Mutex
	cumulative int64
	active     int
	buckets    [transferStatsBucketCount]transferBucket
	now        func() time.Time
}

func (stats *transferWindow) currentTime() time.Time {
	if stats.now != nil {
		return stats.now()
	}
	return time.Now()
}

func (stats *transferWindow) record(bytes int) {
	if bytes <= 0 {
		return
	}
	now := stats.currentTime()
	period := now.UnixNano() / int64(transferStatsBucket)
	index := int(period % int64(transferStatsBucketCount))
	if index < 0 {
		index += transferStatsBucketCount
	}

	stats.mu.Lock()
	stats.cumulative += int64(bytes)
	bucket := &stats.buckets[index]
	if bucket.period != period {
		bucket.period = period
		bucket.bytes = 0
	}
	bucket.bytes += int64(bytes)
	stats.mu.Unlock()
}

func (stats *transferWindow) begin() func() {
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			stats.mu.Lock()
			stats.active--
			stats.mu.Unlock()
		})
	}
}

func (stats *transferWindow) snapshot() (cumulative int64, rate float64, active int) {
	now := stats.currentTime()
	threshold := now.Add(-transferStatsWindow)
	oldest := time.Time{}
	hasOldest := false
	var recent int64

	stats.mu.Lock()
	defer stats.mu.Unlock()
	for _, bucket := range stats.buckets {
		if bucket.bytes == 0 {
			continue
		}
		bucketTime := time.Unix(0, bucket.period*int64(transferStatsBucket))
		if bucketTime.Before(threshold) {
			continue
		}
		recent += bucket.bytes
		if !hasOldest || bucketTime.Before(oldest) {
			oldest = bucketTime
			hasOldest = true
		}
	}
	cumulative = stats.cumulative
	active = stats.active
	if recent == 0 {
		return cumulative, 0, active
	}

	// A single short transfer still has a useful rate sample; the bucket width
	// limits the initial estimate while the denominator grows during idle time.
	elapsed := now.Sub(oldest)
	if elapsed < transferStatsBucket {
		elapsed = transferStatsBucket
	}
	if elapsed > transferStatsWindow {
		elapsed = transferStatsWindow
	}
	rate = float64(recent) / elapsed.Seconds()
	return cumulative, rate, active
}

type transferReader struct {
	reader io.Reader
	stats  *transferWindow
}

func (reader *transferReader) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	reader.stats.record(n)
	return n, err
}

type transferReadCloser struct {
	reader io.ReadCloser
	stats  *transferWindow
	finish func()
	once   sync.Once
}

func (reader *transferReadCloser) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	reader.stats.record(n)
	if err != nil {
		reader.finishOnce()
	}
	return n, err
}

func (reader *transferReadCloser) Close() error {
	err := reader.reader.Close()
	reader.finishOnce()
	return err
}

func (reader *transferReadCloser) finishOnce() {
	reader.once.Do(reader.finish)
}
