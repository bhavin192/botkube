package mattermost

import (
	"fmt"
	"strings"

	"github.com/infracloudio/botkube/pkg/config"
	"github.com/infracloudio/botkube/pkg/execute"
	"github.com/infracloudio/botkube/pkg/logging"
	"github.com/mattermost/mattermost-server/model"
)

var client *model.Client4
var webSocketClient *model.WebSocketClient
var botUser *model.User
var botTeam *model.Team
var botChannel *model.Channel

const (
	// BotName stores Botkube details
	BotName = "botkube"
	// ChannelPurpose describes the purpose of Botkube channel
	ChannelPurpose = "Botkube alerts"
	// WebSocketProtocol stores protocol initials for web socket
	WebSocketProtocol = "ws:"
	// Logs file name
	Logs = "logs"
)

// Bot listens for user's message, execute commands and sends back the response
type Bot struct {
	ServerURL    string
	Token        string
	TeamName     string
	ChannelName  string
	ClusterName  string
	AllowKubectl bool
}

// NewMattermostBot returns new Bot object
func NewMattermostBot() *Bot {
	c, err := config.New()
	if err != nil {
		logging.Logger.Fatal(fmt.Sprintf("Error in loading configuration. Error:%s", err.Error()))
	}

	return &Bot{
		ServerURL:    c.Communications.Mattermost.URL,
		Token:        c.Communications.Mattermost.Token,
		TeamName:     c.Communications.Mattermost.Team,
		ChannelName:  c.Communications.Mattermost.Channel,
		ClusterName:  c.Settings.ClusterName,
		AllowKubectl: c.Settings.AllowKubectl,
	}
}

// Channel structure in Mattermost
func mmChannel(channelName, teamID string) *model.Channel {
	return &model.Channel{
		Name:        channelName,
		DisplayName: channelName,
		Purpose:     ChannelPurpose,
		Type:        model.CHANNEL_OPEN,
		TeamId:      teamID,
	}
}

// Start establishes mattermost connection and listens for messages
func (b *Bot) Start() {
	client = model.NewAPIv4Client(b.ServerURL)
	client.SetOAuthToken(b.Token)

	// Check connection to Mattermost server
	err := checkServerConnection()
	if err != nil {
		logging.Logger.Error("There was a problem pinging the Mattermost server. Error: ", err)
		return
	}

	// Check Team exists and get Team ID
	botTeam, err = getBotTeam(b.TeamName)
	if err != nil {
		logging.Logger.Error("There was a problem finding Mattermost team. Error: ", err)
		return
	}

	// Check Botkube user exists and get User ID
	botUser, err = getBotUser(botTeam.Id)
	if err != nil {
		logging.Logger.Error("There was a problem creating user in Mattermost. Error: ", err)
		return
	}

	// Check Channel exists or create Channel and add user to the Channel
	botChannel, err = getBotChannel(b.ChannelName, botTeam.Id, botUser.Id)
	if err != nil {
		logging.Logger.Error("There was a problem creating channel. Error: ", err)
		return
	}

	// Create WebSocketClient and handle messages
	webSocketURL := WebSocketProtocol + strings.SplitN(b.ServerURL, ":", 2)[1]
	webSocketClient, _ := model.NewWebSocketClient4(webSocketURL, client.AuthToken)
	webSocketClient.Listen()
	go func() {
		for {
			select {
			case event := <-webSocketClient.EventChannel:
				handleMessage(event, b.AllowKubectl, b.ClusterName, b.ChannelName)
			}
		}
	}()
	return
}

// Check if Mattermost server is reachable
func checkServerConnection() error {
	if _, resp := client.GetOldClientConfig(""); resp.Error != nil {
		return resp.Error
	}
	return nil
}

// Check if team exists in Mattermost
func getBotTeam(teamName string) (*model.Team, error) {
	botTeam, resp := client.GetTeamByName(teamName, "")
	if resp.Error != nil {
		return botTeam, resp.Error
	}
	return botTeam, nil
}

// Check if botkube user exists in Mattermost
func getBotUser(teamID string) (*model.User, error) {
	users, resp := client.AutocompleteUsersInTeam(teamID, BotName, "")
	if resp.Error != nil {
		return nil, resp.Error
	}
	return users.Users[0], nil
}

// Create channel if not present and add botkube user in channel
func getBotChannel(channelName, botTeamID, botUserID string) (*model.Channel, error) {
	// Checking if channel exists
	botChannel, resp := client.GetChannelByName(channelName, botTeamID, "")
	if resp.Error != nil {
		// Creating channel
		if botChannel, resp = client.CreateChannel(mmChannel(channelName, botTeamID)); resp.Error != nil {
			return botChannel, resp.Error
		}
	}
	logging.Logger.Info("Channel created successfully in Mattermost")

	// Adding Botkube user to channel
	client.AddChannelMember(botChannel.Id, botUserID)
	return botChannel, nil
}

// Check incomming message and take action
func handleMessage(event *model.WebSocketEvent, allowkubectl bool, clusterName, channelName string) {
	// Check incomming message event type
	if event.Event != model.WEBSOCKET_EVENT_POSTED {
		return
	}
	post := model.PostFromJson(strings.NewReader(event.Data["post"].(string)))

	// Check if message posted by botkube and has @botkube prefix
	if post.UserId == botUser.Id || !(strings.HasPrefix(post.Message, "@"+BotName+" ")) {
		return
	}
	inMessage := strings.TrimPrefix(post.Message, "@"+BotName+" ")

	// Check where the message is posted
	isAuthChannel := false
	if event.Broadcast.ChannelId == botChannel.Id {
		isAuthChannel = true
	}

	e := execute.NewDefaultExecutor(inMessage, allowkubectl, clusterName, channelName, isAuthChannel)
	outMessage := e.Execute()
	if len(outMessage) == 0 {
		logging.Logger.Info("Invalid request. Dumping the response")
		return
	}
	sendMessage("`"+outMessage+"`", post.Id, event.Broadcast.ChannelId)
}

// Send messages to Mattermost
func sendMessage(msg, postID, channelID string) {
	post := &model.Post{}
	post.ChannelId = channelID
	post.RootId = postID
	// Create file if message is too large
	if len(msg) >= 3990 {
		res, resp := client.UploadFileAsRequestBody([]byte(msg), channelID, Logs)
		if resp.Error != nil {
			logging.Logger.Error("Error occured while uploading file. Error: ", resp.Error)
		}
		post.FileIds = []string{string(res.FileInfos[0].Id)}
	} else {
		post.Message = msg
	}

	// Create a post in the Channel
	if _, resp := client.CreatePost(post); resp.Error != nil {
		logging.Logger.Error("Failed to send message. Error: ", resp.Error)
	}
}
