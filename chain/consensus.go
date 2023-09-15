package chain

type ConsensusName string

const (
	AuRaConsensus   ConsensusName = "aura"
	EtHashConsensus ConsensusName = "ethash"
	ParliaConsensus ConsensusName = "parlia"
	CliqueConsensus ConsensusName = "clique"
	BorConsensus    ConsensusName = "bor"
)
