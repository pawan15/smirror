package smirror

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/viant/afs"
	"github.com/viant/afs/file"
	"github.com/viant/afs/url"
	"github.com/viant/afsc/gs"
	"io"
	"io/ioutil"
	"smirror/config"
	"smirror/job"
	"smirror/msgbus"
	"smirror/msgbus/pubsub"
	"smirror/secret"
	"sync"
	"sync/atomic"
	"time"
)

//Service represents a mirror service
type Service interface {
	//Mirror copies/split source to matched destination
	Mirror(ctx context.Context, request *Request) *Response
}

type service struct {
	mux *sync.Mutex
	config *Config
	afs.Service
	secret secret.Service
	msgbus msgbus.Service
}

func (s *service) Mirror(ctx context.Context, request *Request) *Response {
	response := NewResponse()
	if err := s.mirror(ctx, request, response); err != nil {
		response.Status = StatusError
		response.Error = err.Error()
	}
	response.TimeTakenMs = int(time.Now().Sub(response.startTime) / time.Millisecond)
	return response

}

func (s *service) mirror(ctx context.Context, request *Request, response *Response) (err error) {
	hasChanges, err := s.config.Meta.HasChanged(ctx, s.Service)
	if err != nil {
		return err
	}
	if hasChanges {
		if err = s.Init(ctx);err != nil {
			return err
		}
	}

	route := s.config.Routes.HasMatch(request.URL)
	if route == nil {
		response.Status = StatusNoMatch
		return nil
	}
	if route.Split != nil {
		err = s.mirrorChunkedAsset(ctx, route, request, response)
	} else {
		err = s.mirrorAsset(ctx, route, request.URL, response)
	}
	jobContent := job.NewContext(ctx, err, request.URL)
	if e := route.Actions.Run(jobContent, s.Service); e != nil && err == nil {
		err = e
	}
	return err
}

func (s *service) mirrorAsset(ctx context.Context, route *config.Route, URL string, response *Response) error {
	options, err := s.secret.StorageOpts(ctx, route.Source)
	if err != nil {
		return err
	}
	reader, err := s.Service.DownloadWithURL(ctx, URL, options...)
	if err != nil {
		return err
	}

	sourceCompression := config.NewCompressionForURL(URL)
	destCompression := route.Compression
	if sourceCompression.Equals(destCompression) {
		sourceCompression = nil
		destCompression = nil
	}
	reader, err = NewReader(reader, sourceCompression)
	if err != nil {
		return err
	}
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	destName := route.Name(URL)
	destURL := url.Join(route.Dest.URL, destName)

	if reader == nil {
		return fmt.Errorf("reader was empty")
	}

	dataCopy := &Transfer{
		Resource: &route.Dest,
		Reader:   reader,
		Dest:     NewDatafile(destURL, destCompression)}
	return s.transfer(ctx, dataCopy, response)
}

func (s *service) mirrorChunkedAsset(ctx context.Context, route *config.Route, request *Request, response *Response) error {
	options, err := s.secret.StorageOpts(ctx, route.Source)
	if err != nil {
		return err
	}
	reader, err := s.Service.DownloadWithURL(ctx, request.URL, options...)
	if err == nil {
		reader, err = NewReader(reader, config.NewCompressionForURL(request.URL))
		if err != nil {
			return err
		}
	}
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	counter := int32(0)
	waitGroup := &sync.WaitGroup{}
	err = Split(reader, s.chunkWriter(ctx, request.URL, route, &counter, waitGroup, response), route.Split.MaxLines)
	if err == nil {
		waitGroup.Wait()
	}
	return err
}

func (s *service) transfer(ctx context.Context, transfer *Transfer, response *Response) error {
	if transfer.Resource.Topic != "" {
		return s.publish(ctx, transfer, response)
	}
	if transfer.Resource.URL != "" {
		return s.upload(ctx, transfer, response)
	}
	return fmt.Errorf("invalid transfer: %v", transfer)
}

func (s *service) publish(ctx context.Context, transfer *Transfer, response *Response) error {
	reader, err := transfer.GetReader()
	if err != nil {
		return err
	}
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return err
	}
	switch s.config.SourceScheme {
	case gs.Scheme:
		attributes := make(map[string]interface{})
		attributes["Dest"] = transfer.Dest.URL
		messageIDs, err := s.msgbus.Publish(ctx, transfer.Resource.Topic, data, attributes)
		if err != nil {
			return err
		}
		response.MessageIDs = append(response.MessageIDs, messageIDs...)
		return nil
	}
	return fmt.Errorf("unsupported message msgbus %v", s.config.SourceScheme)
}

func (s *service) upload(ctx context.Context, transfer *Transfer, response *Response) error {
	reader, err := transfer.GetReader()
	if err != nil {
		return err
	}
	options, err := s.secret.StorageOpts(ctx, transfer.Resource)
	if err != nil {
		return err
	}
	if err = s.Service.Upload(ctx, transfer.Dest.URL, file.DefaultFileOsMode, reader, options...); err != nil {
		return err
	}
	response.AddURL(transfer.Dest.URL)
	return nil
}

//Init initialises this service
func (s *service) Init(ctx context.Context) error {
	if err := s.config.loadRoutes(ctx, s.Service);err != nil {
		return err
	}
	resources, err := s.config.Resources(ctx, s.Service)
	if err != nil {
		return  err
	}
	if len(resources) > 0 {
		if err = s.secret.Init(ctx, s.Service, resources);err != nil {
			return errors.Wrap(err, "failed to init resource secrets")
		}
	}
	if s.config.UseMessageDest() && s.msgbus == nil {
		if s.config.SourceScheme == gs.Scheme {
			if s.msgbus, err = pubsub.New(ctx); err != nil {
				return errors.Wrapf(err, "unable to create publisher for %v", s.config.SourceScheme)
			}
		}
	}
	return nil
}


func (s *service) chunkWriter(ctx context.Context, URL string, route *config.Route, counter *int32, waitGroup *sync.WaitGroup, response *Response) func() io.WriteCloser {
	return func() io.WriteCloser {
		splitCount := atomic.AddInt32(counter, 1)
		destName := route.Split.Name(route, URL, splitCount)
		destURL := url.Join(route.Dest.URL, destName)
		return NewWriter(route, func(writer *Writer) error {
			if writer.Reader == nil {
				return fmt.Errorf("writer reader was empty")
			}
			waitGroup.Add(1)
			defer waitGroup.Done()
			dataCopy := &Transfer{
				Resource: &route.Dest,
				Reader:   writer.Reader,
				Dest:     NewDatafile(destURL, nil),
			}
			return s.transfer(ctx, dataCopy, response)
		})
	}
}

//New creates a new mirror service
func New(ctx context.Context, config *Config) (Service, error) {
	err := config.Init(ctx, afs.New())
	if err != nil {
		return nil, err
	}
	fs := afs.New()
	result := &service{config: config,
		Service: fs,
		mux: &sync.Mutex{},
		secret:  secret.New(config.SourceScheme),
	}
	return result, result.Init(ctx)
}
