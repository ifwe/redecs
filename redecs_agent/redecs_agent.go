package main

// Originally adapted from https://github.com/awslabs/service-discovery-ecs-dns
// This service is intended to run on AWS ECS instances. It will look for special
// SERVICE environment variables and add those services to a Redis ZSET with the
// timestamp as the key. The accompanying service, red53_agent will check this
// ZSET and write changes to AWS Route53.

import (
	docker_router "redecs/docker_router"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/fsouza/go-dockerclient"
	"gopkg.in/redis.v5"
	"os"
	"strconv"
	"strings"
	"time"
)

const checkInterval = 30 * time.Second // how often to check Docker

type config struct {
	LocalIp    string
	RedisHost  string
}

var configuration config

func logErrorAndFail(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func logErrorNoFatal(err error) {
	if err != nil {
		log.Error(err)
	}
}

var dockerClient *docker.Client
var redisClient *redis.Client

// This reports an instance as active. It adds it to the redecs:service_pings set
// and publishes a redecs:service_channel event.
func reportServiceActive(serviceName string) error {
	log.Debugf("Reporting %s as active with IP %s", serviceName, configuration.LocalIp)
	value := serviceName + "_" + configuration.LocalIp
	// write {timestamp => SERVICENAME_IP} to redecs:service_pings
	err := redisClient.ZAdd("redecs:service_pings", redis.Z{float64(time.Now().Unix()), value}).Err()
	if err != nil {
		return err
	}
	// notify the channel that this service has been reported active
	err = redisClient.Publish("redecs:service_channel", "+"+value).Err()
	return err
}

// This reports an instance as inactive. It removes it from the redecs:service_pings set
// and publishes a redecs:service_channel event.
func reportServiceInactive(serviceName string) error {
	log.Debugf("Reporting %s as inactive with IP %s", serviceName, configuration.LocalIp)
	value := serviceName + "_" + configuration.LocalIp
	// remove SERVICENAME_IP from redecs:service_pings
	err := redisClient.ZRem("redecs:service_pings", value).Err()
	if err != nil {
		return err
	}
	// notify the channel that this service has been reported active
	err = redisClient.Publish("redecs:service_channel", "-"+value).Err()
	return err
}

// Gets the service name from a Docker container environment variable, if present.
func getServiceName(container *docker.Container) string {
	// One of the environment variables should be SERVICE_<port>_NAME = <name of the service>
	// We look for this environment variable doing a split in the "=" and another one in the "_"
	// So envEval = [SERVICE_<port>_NAME, <name>]
	// nameEval = [SERVICE, <port>, NAME]
	for _, env := range container.Config.Env {
		envEval := strings.Split(env, "=")
		nameEval := strings.Split(envEval[0], "_")
		if len(envEval) == 2 && len(nameEval) == 3 && nameEval[0] == "SERVICE" && nameEval[2] == "NAME" {
			if _, err := strconv.Atoi(nameEval[1]); err == nil {
				return envEval[1]
			}
		}
	}
	return ""
}

func processContainers() {
	log.Debug("Listing active containers...")

	// only get running ones
	containers, err := dockerClient.ListContainers(docker.ListContainersOptions{All: false})
	// bail out completely if we can't list containers.
	logErrorAndFail(err)
	for _, containerEntry := range containers {
		// have to inspect to get the environment variable
		container, err := dockerClient.InspectContainer(containerEntry.ID)
		// retry logic?
		logErrorAndFail(err)
		serviceName := getServiceName(container)
		if serviceName != "" {
			reportServiceActive(serviceName)
		}
	}

	log.Debug("Done checking active containers")
}

func main() {
	var err error
	var sum int
	if len(os.Args) < 2 || len(os.Args) > 3 {
		err = errors.New(fmt.Sprintf("Usage: %s [Redis host] {host IP (for testing)}\n", os.Args[0]))
		logErrorAndFail(err)
	}
	if len(os.Args) == 3 {
		configuration.LocalIp = os.Args[2]
	} else {
		metadataClient := ec2metadata.New(session.New())
		localIp, err := metadataClient.GetMetadata("/local-ipv4")
		logErrorAndFail(err)
		configuration.LocalIp = localIp
	}
	configuration.RedisHost = os.Args[1]
	endpoint := "unix:///var/run/docker.sock"
	dockerClient, err = docker.NewClient(endpoint)

	// bail out if we can't talk to Docker
	logErrorAndFail(err)

	sum = 1
	for {
		// We try to get the Redis connection using exponential backoff
		redisClient = redis.NewClient(&redis.Options{
			Addr:       configuration.RedisHost + ":6379",
			Password:   "", // none
			DB:         0,  // default DB
			MaxRetries: 3,
		})
		// Check the connection.
		err := redisClient.Ping().Err()

		if err == nil {
			break
		}
		if sum > 8 {
			logErrorAndFail(err)
		}
		time.Sleep(time.Duration(sum) * time.Second)
		sum += 2
	}

	startFn := func(event *docker.APIEvents) error {
		var err error
		container, err := dockerClient.InspectContainer(event.ID)
		logErrorAndFail(err)
		service := getServiceName(container)
		if service != "" {
			sum = 1
			for {
				if err = reportServiceActive(service); err == nil {
					break
				}
				if sum > 8 {
					log.Errorf("Error reporting service %s active", service)
					break
				}
				time.Sleep(time.Duration(sum) * time.Second)
				sum += 2
			}
		}
		log.Debugf("Docker %s started", event.ID)
		return nil
	}

	stopFn := func(event *docker.APIEvents) error {
		var err error
		container, err := dockerClient.InspectContainer(event.ID)
		logErrorAndFail(err)
		service := getServiceName(container)
		if service != "" {
			sum = 1
			for {
				if err = reportServiceInactive(service); err == nil {
					break
				}
				if sum > 8 {
					log.Errorf("Error reporting service %s inactive", service)
					break
				}
				time.Sleep(time.Duration(sum) * time.Second)
				sum += 2
			}
		}
		log.Debugf("Docker %s stopped", event.ID)
		return nil
	}

	dockerRouter, err := docker_router.AddStartStopHandlers(startFn, stopFn, dockerClient)
	defer dockerRouter.Stop()
	logErrorAndFail(err)
	dockerRouter.Start()

	log.Debug("Waiting for Docker events")

	// continue processing once per minute
	ticker := time.NewTicker(checkInterval)

	for {
		processContainers()
		<-ticker.C
	}
}
