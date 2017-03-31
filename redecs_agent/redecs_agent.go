package main

// Originally adapted from https://github.com/awslabs/service-discovery-ecs-dns
// This service is intended to run on AWS ECS instances. It will look for special
// SERVICE environment variables and add those services to a Redis ZSET with the
// timestamp as the key. The accompanying service, red53_agent will check this
// ZSET and write changes to AWS Route53.

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"gopkg.in/redis.v5"
	"os"
	"time"
)

const reportInterval = 30 * time.Second // how often to report

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

var redisClient *redis.Client

// This reports an instance as active. It adds it to the redecs:service_pings set
// and publishes a redecs:service_channel event.
func reportActiveService(serviceName string) error {
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

// Gets the service name from the SERVICE_NAME environment variable.
func getServiceName() string {
	return os.Getenv("SERVICE_NAME")
}

func main() {
	var err error
	var sum int
	if len(os.Args) < 2 || len(os.Args) > 3 {
		err = errors.New(fmt.Sprintf("Usage: %s [Redis host] {IP for testing purposes}\n", os.Args[0]))
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

	serviceName := getServiceName()
	if serviceName == "" {
		err = errors.New("'SERVICE_NAME' environment variable is not defined! Exiting.")
		logErrorAndFail(err)
	}

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

	log.Debug("redecs_agent started")

	// continue processing once per minute
	ticker := time.NewTicker(reportInterval)

	for {
		reportActiveService(serviceName)
		<-ticker.C
	}
}
