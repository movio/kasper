package kasper

import (
	"log"
	"github.com/Shopify/sarama"
)

type TopicProcessor struct {
	config              *TopicProcessorConfig
	containerId         int
	client              sarama.Client
	offsetManager       sarama.OffsetManager
	partitionProcessors []*partitionProcessor
	inputTopics         []string
	partitions          []int32
}

func partitionsOfTopics(topics []string, client sarama.Client) []int32 {
	partitionsSet := make(map[int32]struct{})
	for _, topic := range topics {
		partitions, err := client.Partitions(topic)
		if err != nil {
			log.Fatal(err)
		}
		for _, partition := range partitions {
			partitionsSet[partition] = struct{}{}
		}
	}
	i := 0
	partitions := make([]int32, len(partitionsSet))
	for partition := range partitionsSet {
		partitions[i] = partition
		i++
	}
	return partitions
}

func NewTopicProcessor(config *TopicProcessorConfig, makeProcessor func() MessageProcessor, containerId int) *TopicProcessor {
	// TODO: check all input topics are covered by a Serde
	// TODO: check all input partitions and make sure PartitionAssignment is valid
	// TODO: check containerId is within [0, ContainerCount)
	inputTopics := config.InputTopics
	brokerList := config.BrokerList
	client, err := sarama.NewClient(brokerList, sarama.NewConfig())
	if err != nil {
		log.Fatal(err)
	}
	partitions := config.partitionsForContainer(containerId)
	offsetManager, err := sarama.NewOffsetManagerFromClient("kasper-consumer-group" /* FIXME: use TopicProcessorName + ContainerID*/ , client)
	if err != nil {
		log.Fatal(err)
	}
	partitionProcessors := make([]*partitionProcessor, len(partitions))
	topicProcessor := TopicProcessor{
		config,
		containerId,
		client,
		offsetManager,
		partitionProcessors,
		inputTopics,
		partitions,
	}
	for i, partition := range partitions {
		processor := makeProcessor()
		partitionProcessors[i] = newPartitionProcessor(&topicProcessor, processor, partition)
	}
	return &topicProcessor
}

func (tp *TopicProcessor) Run() {
	multiplexed := make(chan *sarama.ConsumerMessage)
	for _, ch := range tp.messageChannels() {
		go func(c <-chan *sarama.ConsumerMessage) {
			for msg := range c {
				multiplexed <- msg
			}
		}(ch)
	}
	for {
		log.Println("Topic Processor is waiting for a message\n")
		message := <-multiplexed
		log.Printf("Got message: %#v\n", message)
		pp := tp.partitionProcessors[message.Partition]
		topicSerde, ok := pp.topicProcessor.config.TopicSerdes[message.Topic]
		if !ok {
			log.Fatalf("Could not find Serde for topic '%s'", message.Topic)
		}
		envelope := IncomingMessage{
			Topic:     message.Topic,
			Partition: message.Partition,
			Offset:    message.Offset,
			Key:       topicSerde.KeySerde.Deserialize(message.Key),
			Value:     topicSerde.ValueSerde.Deserialize(message.Value),
			Timestamp: message.Timestamp,
		}
		pp.messageProcessor.Process(envelope, pp.sender, pp.coordinator)
		pp.offsetManagers[message.Partition].MarkOffset(message.Offset + 1, "")
	}
}

func (tp *TopicProcessor) messageChannels() []<-chan *sarama.ConsumerMessage {
	var chans []<-chan *sarama.ConsumerMessage
	for _, partitionProcessor := range tp.partitionProcessors {
		partitionChannels := partitionProcessor.messageChannels()
		for _, ch := range partitionChannels {
			chans = append(chans, ch)
		}
	}
	return chans
}