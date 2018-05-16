package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/lawrencegripper/ion/internal/app/dispatcher/messaging" //TODO couldn't it be moved into internal/pkg ?
	"github.com/lawrencegripper/ion/internal/app/dispatcher/providers" //TODO couldn't it be moved into internal/pkg ?
	"github.com/lawrencegripper/ion/internal/pkg/servicebus"
	"github.com/lawrencegripper/ion/internal/pkg/types"

	log "github.com/sirupsen/logrus"
)

// Run will start the dispatcher server and wait for new AMQP messages
func Run(cfg *types.Configuration) {
	ctx := context.Background()

	listener := servicebus.NewListener(ctx, cfg)
	sidecarArgs := providers.GetSharedSidecarArgs(cfg, listener.AccessKeys)

	var provider providers.Provider

	if cfg.AzureBatch != nil {
		log.Info("Using Azure batch provider...")
		batchProvider, err := providers.NewAzureBatchProvider(cfg, sidecarArgs)
		if err != nil {
			log.WithError(err).Panic("Couldn't create azure batch provider")
		}
		provider = batchProvider
	} else {
		log.Info("Defaulting to using Kubernetes provider...")
		k8sProvider, err := providers.NewKubernetesProvider(cfg, sidecarArgs)
		if err != nil {
			log.WithError(err).Panic("Couldn't create kubernetes provider")
		}
		provider = k8sProvider
	}

	var wg sync.WaitGroup

	wg.Add(2)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			message, err := listener.AmqpReceiver.Receive(ctx)
			if err != nil {
				// Todo: Investigate the type of error here. If this could be triggered by a poisened message
				// app shouldn't panic.
				log.WithError(err).Panic("Error received dequeuing message")
			}

			if message == nil {
				log.WithError(err).Panic("Error received dequeuing message - nil message")
			}

			log.WithField("message", message).Debug("message received")

			err = provider.Dispatch(messaging.NewAmqpMessageWrapper(message))
			if err != nil {
				log.WithError(err).Error("Couldn't dispatch message to kubernetes provider")
			}

			log.WithField("message", message).Debug("message dispatched")
		}
	}(&wg)

	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			log.Debug("reconciling...")

			err := provider.Reconcile()
			if err != nil {
				// Todo: Should this panic here? Should we tolerate a few failures (k8s upgade causing masters not to be vailable for example?)
				log.WithError(err).Panic("Failed to reconcile ....")
			}
			time.Sleep(time.Second * 15)
		}
	}(&wg)
	wg.Wait()

	//init flaeg
	//flaeg := flaeg.New(rootCmd, os.Args[1:])

	//run test
	//if err := flaeg.Run(); err != nil {
	//	fmt.Printf("Error %s \n", err.Error())
	//}
}