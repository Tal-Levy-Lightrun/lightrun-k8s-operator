package controller

type Tag struct {
	Name string `json:"name"`
}

type Registration struct {
	DisplayName string `json:"displayName"`
	Tags        []Tag  `json:"tags"`
}

type AgentMetadata struct {
	Registration Registration `json:"registration"`
}

func newAgentMetadata() AgentMetadata {
	return AgentMetadata{
		Registration: Registration{
			DisplayName: "",
			Tags:        []Tag{},
		},
	}
}
