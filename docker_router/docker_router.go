package docker_router

import (
	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
	"time"
)

const workerTimeout = 180 * time.Second

type handler interface {
	Handle(*docker.APIEvents) error
}

type DockerRouter struct {
	handlers      map[string][]handler
	dockerClient  *docker.Client
	listener      chan *docker.APIEvents
	workers       chan *worker
	workerTimeout time.Duration
}

func dockerEventsRouter(bufferSize int, workerPoolSize int, dockerClient *docker.Client,
	handlers map[string][]handler) (*DockerRouter, error) {
	workers := make(chan *worker, workerPoolSize)
	for i := 0; i < workerPoolSize; i++ {
		workers <- &worker{}
	}

	DockerRouter := &DockerRouter{
		handlers:      handlers,
		dockerClient:  dockerClient,
		listener:      make(chan *docker.APIEvents, bufferSize),
		workers:       workers,
		workerTimeout: workerTimeout,
	}

	return DockerRouter, nil
}

func (e *DockerRouter) Start() error {
	go e.manageEvents()
	return e.dockerClient.AddEventListener(e.listener)
}

func (e *DockerRouter) Stop() error {
	if e.listener == nil {
		return nil
	}
	return e.dockerClient.RemoveEventListener(e.listener)
}

func (e *DockerRouter) manageEvents() {
	for {
		event := <-e.listener
		timer := time.NewTimer(e.workerTimeout)
		gotWorker := false
		// Wait until we get a free worker or a timeout
		// there is a limit in the number of concurrent events managed by workers to avoid resource exhaustion
		// so we wait until we have a free worker or a timeout occurs
		for !gotWorker {
			select {
			case w := <-e.workers:
				if !timer.Stop() {
					<-timer.C
				}
				go w.doWork(event, e)
				gotWorker = true
			case <-timer.C:
				log.Infof("Timed out waiting.")
			}
		}
	}
}

type worker struct{}

func (w *worker) doWork(event *docker.APIEvents, e *DockerRouter) {
	defer func() { e.workers <- w }()
	if handlers, ok := e.handlers[event.Status]; ok {
		log.Infof("Processing event: %#v", event)
		for _, handler := range handlers {
			if err := handler.Handle(event); err != nil {
				log.Errorf("Error processing event %#v. Error: %v", event, err)
			}
		}
	}
}

type dockerHandler struct {
	handlerFunc func(event *docker.APIEvents) error
}

func (th *dockerHandler) Handle(event *docker.APIEvents) error {
	return th.handlerFunc(event)
}

func (th *dockerHandler) SetHandler(handlerFunc func(event *docker.APIEvents) error) {
	th.handlerFunc = handlerFunc
}

func AddStartStopHandlers(startFn func(event *docker.APIEvents) error, stopFn func(event *docker.APIEvents) error, dockerClient *docker.Client) (*DockerRouter, error) {
	startHandler := &dockerHandler{
		handlerFunc: startFn,
	}

	stopHandler := &dockerHandler{
		handlerFunc: stopFn,
	}

	handlers := map[string][]handler{"start": []handler{startHandler}, "die": []handler{stopHandler}}
	router, err := dockerEventsRouter(5, 5, dockerClient, handlers)
	return router, err
}
