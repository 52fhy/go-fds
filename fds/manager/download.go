package manager

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"

	"github.com/sirupsen/logrus"
	"github.com/v2tool/galaxy-fds-sdk-go/fds"
	"github.com/v2tool/galaxy-fds-sdk-go/fds/httpparser"
)

// Downloader is a FDS client for file concurrency download
type Downloader struct {
	logger *logrus.Logger
	client *fds.Client

	PartSize    int64
	Concurrency int
	Breakpoint  bool
}

// NewDownloader new a downloader
func NewDownloader(client *fds.Client, partSize int64, concurrency int, breakpoint bool) *Downloader {
	downloader := &Downloader{
		PartSize:    partSize,
		Concurrency: concurrency,
		Breakpoint:  breakpoint,

		client: client,
	}
	downloader.logger = logrus.New()
	downloader.logger.SetLevel(logrus.WarnLevel)

	return downloader
}

type breakpointInfo struct {
	FilePath   string
	BucketName string
	ObjectName string
	ObjectStat objectStat
	Parts      []part
	PartStat   []bool
	Start      int64
	End        int64
	MD5        string

	downloader *Downloader
}

type objectStat struct {
	Size         int64  // Object size
	LastModified string // Last modified time
}

func (bp *breakpointInfo) Load(path string) error {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, bp)
}

func (bp *breakpointInfo) Dump() error {
	bpi := *bp

	bpi.MD5 = ""
	data, err := json.Marshal(bpi)
	if err != nil {
		return err
	}

	sum := md5.Sum(data)
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	bpi.MD5 = b64

	data, err = json.Marshal(bpi)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(bpi.FilePath, data, os.FileMode(0664))
}

func (bp *breakpointInfo) Validate(bucketName, objectName string, r httpparser.HTTPRange) error {
	if bucketName != bp.BucketName || objectName != bp.ObjectName {
		return fmt.Errorf("BucketName or ObjectName is not matching")
	}

	bpi := *bp
	bpi.MD5 = ""
	data, err := json.Marshal(bpi)
	if err != nil {
		return err
	}
	sum := md5.Sum(data)
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	if b64 != bp.MD5 {
		return fmt.Errorf("MD5 is not matching")
	}

	c := bp.downloader.client
	metadata, err := c.GetObjectMetadata(bucketName, objectName)
	if err != nil {
		return err
	}

	length, err := metadata.GetContentLength()
	if err != nil {
		return err
	}
	if bp.ObjectStat.Size != length || bp.ObjectStat.LastModified != metadata.Get(fds.HTTPHeaderLastModified) {
		return fmt.Errorf("Object state is not matching")
	}

	if bp.Start != r.Start || bp.End != r.End {
		return fmt.Errorf("Range is not matching")
	}

	return nil
}

func (bp *breakpointInfo) UnfinishParts() []part {
	var result []part

	for i, s := range bp.PartStat {
		if !s {
			result = append(result, bp.Parts[i])
		}
	}

	return result
}

func (bp *breakpointInfo) Initilize(downloader *Downloader,
	bucketName, objectName, filePath string, r httpparser.HTTPRange, md *fds.ObjectMetadata) error {
	bp.MD5 = ""
	bp.BucketName = bucketName
	bp.ObjectName = objectName
	bp.FilePath = filePath
	bp.Start = r.Start
	bp.End = r.End
	bp.downloader = downloader

	contentLength, err := md.GetContentLength()
	if err != nil {
		return err
	}

	parts, err := downloader.splitDownloadParts(contentLength, r)
	if err != nil {
		return err
	}
	bp.Parts = parts

	bp.PartStat = make([]bool, len(bp.Parts))

	bp.ObjectStat = objectStat{
		Size:         contentLength,
		LastModified: md.Get(fds.HTTPHeaderLastModified),
	}

	return nil
}

func (bp *breakpointInfo) Destroy() {
	os.Remove(bp.FilePath)
}

// DownloadRequest is the input of Download
type DownloadRequest struct {
	fds.GetObjectRequest
	FilePath           string
	BreakpointFilePath string
}

// Download performs the downloading action
func (downloader *Downloader) Download(request *DownloadRequest) error {
	if downloader.PartSize < 1 {
		return fmt.Errorf("client: part size should not be smaller than 1")
	}

	if downloader.Breakpoint {
		request.BreakpointFilePath = fmt.Sprintf("%s.bp", request.FilePath)
	}

	var parts []part
	var err error

	metadata, err := downloader.client.GetObjectMetadata(request.BucketName, request.ObjectName)
	if err != nil {
		return err
	}

	contentLength, err := strconv.ParseInt(metadata.Get(fds.HTTPHeaderContentMetadataLength), 10, 0)
	if err != nil {
		return err
	}

	ranges, err := httpparser.Range(request.Range)
	if err != nil {
		return err
	}

	if len(ranges) == 0 {
		ranges = append(ranges, httpparser.HTTPRange{End: contentLength})
	}

	if len(ranges) > 1 {
		return fmt.Errorf("fds: does not support (bytes=i-j,m-n) format, only support (bytes=i-j)")
	}

	start := ranges[0].Start
	end := ranges[0].End + 1
	if ranges[0].Start < 0 || ranges[0].Start >= contentLength || ranges[0].End > contentLength || ranges[0].Start > ranges[0].End {
		start = 0
		end = contentLength
	}
	r := httpparser.HTTPRange{
		Start: start,
		End:   end,
	}

	bp := breakpointInfo{
		downloader: downloader,
	}
	if downloader.Breakpoint {
		// load breakpoint info
		err = bp.Load(request.BreakpointFilePath)
		if err != nil {
			bp.Destroy()
		}

		// validate breakpoint info
		err = bp.Validate(request.BucketName, request.ObjectName, r)
		if err != nil {
			downloader.logger.Warn(err)
			downloader.logger.Warn("breakpoint info is invalid")
			bp.Initilize(downloader, request.BucketName, request.ObjectName, request.BreakpointFilePath, r, metadata)
			bp.Destroy()
		}

		// get parts from breakpoint info
		parts = bp.UnfinishParts()
	} else {
		parts, err = downloader.splitDownloadParts(contentLength, r)
		if err != nil {
			return err
		}
	}

	jobs := make(chan part, len(parts))
	results := make(chan part, len(parts))
	failed := make(chan error)
	finished := make(chan bool)

	tmpFilePath := request.FilePath + ".tmp"
	for i := 1; i < downloader.Concurrency; i++ {
		go downloader.consume(i, request, tmpFilePath, jobs, results, failed, finished)
	}

	go downloader.produce(jobs, parts)

	completed := 0
	for completed < len(parts) {
		select {
		case p := <-results:
			completed++
			if downloader.Breakpoint {
				bp.PartStat[p.Index] = true
				bp.Dump()
			}
		case err := <-failed:
			close(finished)
			return err
		}
	}

	if downloader.Breakpoint {
		os.Remove(request.BreakpointFilePath)
	}
	return os.Rename(tmpFilePath, request.FilePath)
}

func (downloader *Downloader) consume(id int,
	request *DownloadRequest, tmpFilePath string, jobs <-chan part, results chan<- part, failed chan<- error, finished <-chan bool) {
	for p := range jobs {
		req := &fds.GetObjectRequest{
			BucketName: request.BucketName,
			ObjectName: request.ObjectName,
			Range:      fmt.Sprintf("bytes=%v-%v", p.Start, p.End),
		}

		data, err := downloader.client.GetObject(req)
		if err != nil {
			downloader.logger.Debug(err.Error())
			failed <- err
			break
		}
		defer data.Close()

		select {
		case <-finished:
			return
		default:
		}

		fd, err := os.OpenFile(tmpFilePath, os.O_WRONLY|os.O_CREATE, os.FileMode(0664))
		if err != nil {
			failed <- err
			break
		}

		_, err = fd.Seek(p.Start-p.Offset, io.SeekStart)
		if err != nil {
			fd.Close()
			failed <- err
			break
		}

		_, err = io.Copy(fd, data)
		if err != nil {
			fd.Close()
			failed <- err
			break
		}

		fd.Close()
		results <- p
	}
}

func (downloader *Downloader) produce(jobs chan part, parts []part) {
	for _, p := range parts {
		jobs <- p
	}
	close(jobs)
}

type part struct {
	Index  int
	Start  int64
	End    int64
	Offset int64
}

func (downloader Downloader) splitDownloadParts(contentLength int64, r httpparser.HTTPRange) ([]part, error) {
	var parts []part

	i := 0
	for offset := r.Start; offset < r.End; offset += downloader.PartSize {
		p := part{
			Index:  i,
			Start:  offset,
			End:    getEnd(offset, r.End, contentLength),
			Offset: r.Start,
		}
		i++
		parts = append(parts, p)
	}

	return parts, nil
}

func getEnd(begin int64, total int64, per int64) int64 {
	if begin+per > total {
		return total - 1
	}
	return begin + per - 1
}