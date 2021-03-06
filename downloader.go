package httpDownloader

import (
	"bufio"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
)

type DownloaderClient struct {
	Speed          int64
	DownloadedSize int64
	Downloading    bool
	Completed      bool
	Failed         bool
	FailedMessage  string
	Info           *DownloaderInfo
	RefreshFunc    func() []string
	RefreshTime    int64

	client      *http.Client
	onCompleted func()
	onFailed    func(err error)
}

func NewClient(info *DownloaderInfo) *DownloaderClient {
	client := &DownloaderClient{
		Info: info,
		client: &http.Client{
			Transport: &http.Transport{
				MaxConnsPerHost:     0,
				MaxIdleConns:        0,
				MaxIdleConnsPerHost: 999,
			},
		},
	}
	return client
}

func (client *DownloaderClient) BeginDownload() error {
	if IsDir(client.Info.TargetFile) {
		return errors.New("target file cannot be dir")
	}
	go func() {
		client.Downloading = true
		for _, block := range client.Info.BlockList {
			client.DownloadedSize += block.DownloadedSize
		}
		threadCount := int(math.Min(float64(client.Info.ThreadCount), float64(len(client.Info.BlockList))))

		ch := make(chan bool)
		for i := 0; i < threadCount; i++ {
			block := client.Info.getNextBlockN()
			if block != -1 {
				client.Info.BlockList[block].Downloading = true
				client.Info.BlockList[block].retryCount = 0
				go client.Info.BlockList[block].download(client, client.Info.Uris[0], ch) //TODO: auto switch uri
			}
		}
		go func() {
			for client.Downloading {
				stat := <-ch
				if stat == false {
					client.Downloading = false
					return
				}
				nextBlock := client.Info.getNextBlockN()
				if nextBlock == -1 {
					if client.Info.allDownloaded() {
						client.Downloading = false
						if client.onCompleted != nil {
							client.onCompleted()
						}
						client.Completed = true
					}
					continue
				}
				client.Info.BlockList[nextBlock].Downloading = true
				client.Info.BlockList[nextBlock].retryCount = 0
				if len(client.Info.Uris) != 0 {
					go client.Info.BlockList[nextBlock].download(client, client.Info.Uris[0], ch)
				}
			}
			close(ch)
		}()
		go func() {
			for client.Downloading {
				oldSize := client.DownloadedSize
				time.Sleep(time.Duration(1) * time.Second)
				client.Speed = client.DownloadedSize - oldSize
			}
		}()
		go func() {
			if client.RefreshFunc != nil && client.RefreshTime != 0 {
				ticker := time.NewTicker(time.Millisecond * time.Duration(client.RefreshTime)).C
				client.Info.Uris = client.RefreshFunc()
				for range ticker {
					if !client.Downloading {
						break
					}
					client.Info.Uris = client.RefreshFunc()
				}
			}
		}()
	}()
	return nil
}

func (block *DownloadBlock) download(client *DownloaderClient, uri string, ch chan bool) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		client.callFailed(err)
		ch <- false
		return
	}
	file, err := os.OpenFile(client.Info.TargetFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		client.callFailed(err)
		ch <- false
		return
	}
	defer file.Close()
	_, err = file.Seek(block.BeginOffset, 0)
	writer := bufio.NewWriter(file)
	defer writer.Flush()
	if err != nil {
		client.callFailed(err)
		ch <- false
		return
	}
	if client.Info.Headers != nil {
		for k, v := range client.Info.Headers {
			req.Header[k] = []string{v}
		}
	}
	req.Header.Set("range", "bytes="+strconv.FormatInt(block.BeginOffset, 10)+"-"+strconv.FormatInt(block.EndOffset, 10))
	resp, err := client.client.Do(req)
	if err != nil {
		client.callFailed(err)
		ch <- false
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if client.RefreshFunc != nil && block.retryCount < 5 {
			block.retryCount++
			client.Info.Uris = client.RefreshFunc()
			block.download(client, client.Info.Uris[0], ch)
			return
		}
		block.Downloading = false
		client.callFailed(errors.New("response status unsuccessful: " + strconv.FormatInt(int64(resp.StatusCode), 10)))
		ch <- false
		return
	}
	var buffer = make([]byte, 1024)
	i, err := resp.Body.Read(buffer)
	for client.Downloading {
		if err != nil && err != io.EOF {
			if block.retryCount < 5 {
				block.retryCount++
				block.download(client, uri, ch)
				return
			}
			block.Downloading = false
			client.callFailed(err)
			ch <- false
			return
		}
		block.retryCount = 0
		i64 := int64(len(buffer[:i]))
		needSize := block.EndOffset + 1 - block.BeginOffset
		if i64 > needSize {
			i64 = needSize
			err = io.EOF
		}
		_, e := writer.Write(buffer[:i64])
		if e != nil {
			block.Downloading = false
			client.callFailed(e)
			ch <- false
			return
		}
		block.BeginOffset += i64
		block.DownloadedSize += i64
		client.DownloadedSize += i64
		if err == io.EOF || block.BeginOffset > block.EndOffset {
			block.Completed = true
			break
		}
		i, err = resp.Body.Read(buffer)
	}
	block.Downloading = false
	ch <- true
}

func (client *DownloaderClient) Pause() {
	if client.Downloading {
		client.Downloading = false
	}
}

func (client *DownloaderClient) OnCompleted(fn func()) {
	client.onCompleted = fn
}

func (client *DownloaderClient) OnFailed(fn func(err error)) {
	client.onFailed = fn
}

func (client *DownloaderClient) callFailed(err error) {
	if client.onFailed != nil && !client.Failed {
		client.Failed = true
		client.FailedMessage = err.Error()
		client.onFailed(err)
	}
}
