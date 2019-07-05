package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	commands "github.com/Bios-Marcel/cmdp"
	"github.com/Bios-Marcel/discordemojimap"
	"github.com/agnivade/levenshtein"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/gorilla/websocket"
)

var (
	lobbyCreatePage    *template.Template
	lobbyPage          *template.Template
	lobbySettingBounds = &SettingBounds{
		MinDrawingTime: 60,
		MaxDrawingTime: 300,
		MinRounds:      1,
		MaxRounds:      20,
		MinMaxPlayers:  2,
		MaxMaxPlayers:  24,
	}
)

// SettingBounds defines the lower and upper bounds for the user-specified
// lobby creation input.
type SettingBounds struct {
	MinDrawingTime int64
	MaxDrawingTime int64
	MinRounds      int64
	MaxRounds      int64
	MinMaxPlayers  int64
	MaxMaxPlayers  int64
}

// CreatePageData defines all non-static data for the lobby create page.
type CreatePageData struct {
	*SettingBounds
	Errors            []string
	Password          string
	DrawingTime       string
	Rounds            string
	MaxPlayers        string
	CustomWords       string
	CustomWordsChance string
}

func createDefaultLobbyCreatePageData() *CreatePageData {
	return &CreatePageData{
		SettingBounds:     lobbySettingBounds,
		DrawingTime:       "120",
		Rounds:            "4",
		MaxPlayers:        "12",
		CustomWordsChance: "50",
	}
}

func init() {
	var err error
	lobbyCreatePage, err = template.New("").ParseFiles("lobby_create.html", "footer.html")
	if err != nil {
		panic(err)
	}

	lobbyPage, err = template.New("").ParseFiles("lobby.html", "lobby_players.html", "lobby_word.html", "footer.html")
	if err != nil {
		panic(err)
	}

	http.HandleFunc("/", HomePage)
	http.HandleFunc("/lobby", ShowLobby)
	http.HandleFunc("/lobby/create", CreateLobby)
	http.HandleFunc("/lobby/players", GetPlayers)
	http.HandleFunc("/lobby/wordhint", GetWordHint)
	http.HandleFunc("/lobby/rounds", GetRounds)
	http.HandleFunc("/ws", wsEndpoint)
}

// HomePage servers the default page for scribble.rs, which is the page to
// create a new lobby.
func HomePage(w http.ResponseWriter, r *http.Request) {
	err := lobbyCreatePage.ExecuteTemplate(w, "lobby_create.html", createDefaultLobbyCreatePageData())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func wsEndpoint(w http.ResponseWriter, r *http.Request) {
	lobbyID := r.URL.Query().Get("id")
	if lobbyID == "" {
		errorPage.ExecuteTemplate(w, "error.html", "The entered URL is incorrect.")
		return
	}

	lobby := GetLobby(lobbyID)

	if lobby == nil {
		errorPage.ExecuteTemplate(w, "error.html", "The lobby does not exist.")
		return
	}

	sessionCookie, noCookieError := r.Cookie("usersession")
	var player *Player
	if noCookieError == nil {
		player = lobby.GetPlayer(sessionCookie.Value)
	} else {
		errorPage.ExecuteTemplate(w, "error.html", "You are not a player of this lobby.")
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Println(player.Name + " has connected")

	player.ws = ws
	player.State = Guessing
	ws.SetCloseHandler(func(code int, text string) error {
		player.State = Disconnected
		player.ws = nil
		return nil
	})

	go wsListen(lobby, player, ws)
}

func wsListen(lobby *Lobby, player *Player, socket *websocket.Conn) {
	for {
		messageType, data, err := socket.ReadMessage()
		if err != nil {
			fmt.Println(err)
			return
		} else if messageType == websocket.TextMessage {
			received := &JSEvent{}
			err := json.Unmarshal(data, received)
			if err != nil {
				socket.Close()
				fmt.Println(err)
				return
			}

			if received.Type == "message" {
				dataAsString, isString := (received.Data).(string)
				if !isString {
					continue
				}

				if strings.HasPrefix(dataAsString, "!") {
					handleCommand(dataAsString[1:], player, lobby)
				} else {
					handleMessage(dataAsString, player, lobby)
				}
			} else if received.Type == "pixel" {
				if lobby.Drawer == player {
					for _, otherPlayer := range lobby.Players {
						if otherPlayer != player && otherPlayer.State != Disconnected && otherPlayer.ws != nil {
							otherPlayer.ws.WriteMessage(websocket.TextMessage, data)
						}
					}
				}
			} else if received.Type == "clear-drawing-board" {
				if lobby.Drawer == player {
					for _, otherPlayer := range lobby.Players {
						if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
							otherPlayer.ws.WriteMessage(websocket.TextMessage, data)
						}
					}
				}
			} else if received.Type == "choose-word" {
				chosenIndex, isInt := (received.Data).(int)
				if !isInt {
					asFloat, isFloat32 := (received.Data).(float64)
					if isFloat32 && asFloat < 4 {
						chosenIndex = int(asFloat)
					} else {
						fmt.Println("Invalid data")
						continue
					}
				}

				if player == lobby.Drawer && len(lobby.WordChoice) > 0 && chosenIndex >= 0 && chosenIndex <= 2 {
					lobby.CurrentWord = lobby.WordChoice[chosenIndex]
					lobby.WordChoice = nil
					lobby.WordHints = createWordHintFor(lobby.CurrentWord)
					lobby.WordHintsShown = showAllInWordHints(lobby.WordHints)
					triggerWordHintUpdate(lobby)
					if lobby.Drawer.State != Disconnected && lobby.Drawer.ws != nil {
						lobby.Drawer.ws.WriteJSON(JSEvent{Type: "your-turn"})
					}
				}
			}
		}
	}
}

func handleMessage(input string, sender *Player, lobby *Lobby) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return
	}

	if lobby.CurrentWord == "" {
		sendMessageToAll(trimmed, sender, lobby)
		return
	}

	if sender.State == Drawing || sender.State == Standby {
		sendMessageToAllNonGuessing(trimmed, sender, lobby)
	} else if sender.State == Guessing {
		lowerCasedInput := strings.ToLower(trimmed)
		lowerCasedSearched := strings.ToLower(lobby.CurrentWord)
		if lowerCasedSearched == lowerCasedInput {
			playerScore := int(math.Ceil(math.Pow(math.Max(float64(lobby.TimeLeft), 1), 1.3) * 2))
			sender.Score += playerScore
			lobby.scoreEarnedByGuessers += playerScore
			sender.State = Standby
			sender.Icon = "✔️"
			if sender.State != Disconnected && sender.ws != nil {
				sender.ws.WriteJSON(JSEvent{Type: "system-message", Data: "You have correctly guessed the word."})
			}

			var someoneStillGuesses bool
			for _, otherPlayer := range lobby.Players {
				if otherPlayer.State == Guessing {
					someoneStillGuesses = true
					break
				}
			}

			if !someoneStillGuesses {
				endRound(lobby)
			} else {
				if sender.State != Disconnected && sender.ws != nil {
					sender.ws.WriteJSON(JSEvent{Type: "update-wordhint"})
				}
				triggerCorrectGuessEvent(lobby)
			}

			return
		} else if levenshtein.ComputeDistance(lowerCasedInput, lowerCasedSearched) == 1 &&
			sender.State != Disconnected && sender.ws != nil {
			sender.ws.WriteJSON(JSEvent{Type: "system-message", Data: fmt.Sprintf("'%s' is very close.", trimmed)})
		}

		sendMessageToAll(trimmed, sender, lobby)
	}
}

func sendMessageToAll(message string, sender *Player, lobby *Lobby) {
	escaped := html.EscapeString(discordemojimap.Replace(message))
	for _, target := range lobby.Players {
		if target.State != Disconnected && target.ws != nil {
			target.ws.WriteJSON(JSEvent{Type: "message", Data: Message{
				Author:  html.EscapeString(sender.Name),
				Content: escaped,
			}})
		}
	}
}

func sendMessageToAllNonGuessing(message string, sender *Player, lobby *Lobby) {
	escaped := html.EscapeString(discordemojimap.Replace(message))
	for _, target := range lobby.Players {
		if target.State != Disconnected && target.State != Guessing && target.ws != nil {
			target.ws.WriteJSON(JSEvent{Type: "non-guessing-player-message", Data: Message{
				Author:  html.EscapeString(sender.Name),
				Content: escaped,
			}})
		}
	}
}

func handleCommand(commandString string, caller *Player, lobby *Lobby) {
	command := commands.ParseCommand(commandString)
	if len(command) >= 1 {
		switch strings.ToLower(command[0]) {
		case "start":
			commandStart(caller, lobby)
		case "kick":
			commandKick(caller, lobby)
		case "setmp":
			commandSetMP(caller, lobby, command)
		case "help":
			//TODO
		case "nick", "name", "username", "nickname", "playername", "alias":
			commandNick(caller, lobby, command)
		}
	}
}

func commandStart(caller *Player, lobby *Lobby) {
	if lobby.Round == 0 && caller == lobby.Owner {
		advanceLobby(lobby)
	}
}

func commandKick(caller *Player, lobby *Lobby) {
	if caller != lobby.Drawer {
		lobby.votekickMapping[caller.UserSession] = true
		if len(lobby.votekickMapping) >= 3 {

			toKick := -1
			for index, otherPlayer := range lobby.Players {
				if otherPlayer == lobby.Drawer {
					toKick = index
					break
				}
			}

			endRound(lobby)

			if toKick != -1 {
				playerToKick := lobby.Players[toKick]
				if playerToKick.ws != nil {
					playerToKick.ws.Close()
					playerToKick.ws = nil
				}
				lobby.Players = append(lobby.Players[:toKick], lobby.Players[toKick+1:]...)
				triggerPlayersUpdate(lobby)
			}
		}
	}
}

func commandNick(caller *Player, lobby *Lobby, args []string) {
	if len(args) == 1 {
		caller.Name = generatePlayerName()
		if caller.State != Disconnected && caller.ws != nil {
			caller.ws.WriteJSON(JSEvent{Type: "reset-username"})
		}
		triggerPlayersUpdate(lobby)
	} else if len(args) == 2 {
		newName := strings.TrimSpace(args[1])
		if len(newName) == 0 {
			caller.Name = generatePlayerName()
			if caller.State != Disconnected && caller.ws != nil {
				caller.ws.WriteJSON(JSEvent{Type: "reset-username"})
			}
			triggerPlayersUpdate(lobby)
		} else if len(newName) <= 30 {
			fmt.Printf("%s is now %s\n", caller.Name, newName)
			caller.Name = newName
			if caller.State != Disconnected && caller.ws != nil {
				caller.ws.WriteJSON(JSEvent{Type: "persist-username", Data: caller.Name})
			}
			triggerPlayersUpdate(lobby)
		}
	}
	//TODO Else, show error
}

func commandSetMP(caller *Player, lobby *Lobby, args []string) {
	if caller == lobby.Owner {
		if len(args) < 2 {
			return
		}

		newMaxPlayersValue := strings.TrimSpace(args[1])
		newMaxPlayersValueInt, err := strconv.ParseInt(newMaxPlayersValue, 10, 64)
		if err == nil {
			if int(newMaxPlayersValueInt) >= len(lobby.Players) && newMaxPlayersValueInt <= lobbySettingBounds.MaxMaxPlayers && newMaxPlayersValueInt >= lobbySettingBounds.MinMaxPlayers {
				lobby.MaxPlayers = int(newMaxPlayersValueInt)

				maxPlayerChangeEvent := &JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value has been changed to %d", lobby.MaxPlayers)}

				for _, otherPlayer := range lobby.Players {
					if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
						otherPlayer.ws.WriteJSON(maxPlayerChangeEvent)
					}
				}

			} else {
				if len(lobby.Players) > int(lobbySettingBounds.MinMaxPlayers) {
					caller.ws.WriteJSON(JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value should be between %d and %d.", len(lobby.Players), lobbySettingBounds.MaxMaxPlayers)})
				} else {
					caller.ws.WriteJSON(JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value should be between %d and %d.", lobbySettingBounds.MinMaxPlayers, lobbySettingBounds.MaxMaxPlayers)})
				}
			}
		} else {
			caller.ws.WriteJSON(JSEvent{Type: "system-message", Data: fmt.Sprintf("MaxPlayers value must be numeric.")})
		}
	} else {
		caller.ws.WriteJSON(JSEvent{Type: "system-message", Data: fmt.Sprintf("Only the lobby owner can change MaxPlayers setting.")})
	}
}

func endRound(lobby *Lobby) {
	overEvent := &JSEvent{Type: "system-message", Data: fmt.Sprintf("Round over. The word was '%s'", lobby.CurrentWord)}

	averageScore := float64(lobby.scoreEarnedByGuessers) / float64(len(lobby.Players)-1)
	if averageScore > 0 {
		lobby.Drawer.Score += int(averageScore * float64(1.1))
	}
	lobby.scoreEarnedByGuessers = 0
	lobby.alreadyUsedWords = append(lobby.alreadyUsedWords, lobby.CurrentWord)
	lobby.CurrentWord = ""

	lobby.WordHints = nil
	lobby.votekickMapping = make(map[string]bool)

	for _, otherPlayer := range lobby.Players {
		if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
			otherPlayer.ws.WriteJSON(overEvent)
		}
	}

	advanceLobby(lobby)
}

func advanceLobby(lobby *Lobby) {
	if lobby.timeLeftTicker != nil {
		lobby.timeLeftTicker.Stop()
		lobby.timeLeftTickerReset <- struct{}{}
	}

	lobby.TimeLeft = lobby.DrawingTime

	for _, otherPlayer := range lobby.Players {
		otherPlayer.State = Guessing
		otherPlayer.Icon = ""
	}

	if lobby.Drawer == nil {
		lobby.Drawer = lobby.Players[0]
		lobby.Round++
	} else {
		if lobby.Drawer == lobby.Players[len(lobby.Players)-1] {
			if lobby.Round == lobby.Rounds {
				lobby.Round = 0

				gameOverEvent := &JSEvent{Type: "system-message", Data: "Game over. Type !start again to start a new round."}

				for _, otherPlayer := range lobby.Players {
					if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
						otherPlayer.ws.WriteJSON(gameOverEvent)
					}
				}

				return
			}

			lobby.Round++
			lobby.Drawer = lobby.Players[0]
		} else {
			selectNextDrawer(lobby)
		}
	}

	lobby.Drawer.State = Drawing
	lobby.Drawer.Icon = "✏️"
	lobby.WordChoice = GetRandomWords(lobby)
	if lobby.Drawer.State != Disconnected && lobby.Drawer.ws != nil {
		lobby.Drawer.ws.WriteJSON(JSEvent{Type: "prompt-words", Data: lobby.WordChoice})
	}

	lobby.timeLeftTicker = time.NewTicker(1 * time.Second)
	go func() {
		showNextHintInSeconds := lobby.DrawingTime / 3
		hintsLeft := 2

		for {
			select {
			case <-lobby.timeLeftTicker.C:
				lobby.TimeLeft--
				triggerTimeLeftUpdate(lobby)
				if hintsLeft > 0 {
					showNextHintInSeconds--
					if showNextHintInSeconds == 0 {
						showNextHintInSeconds = lobby.DrawingTime / 3
						hintsLeft--
						//FIXME If a word is chosen lates, less hints will come overall.
						if lobby.WordHints != nil {
							for {
								randomIndex := rand.Int() % len(lobby.WordHints)
								if !lobby.WordHints[randomIndex].Show {
									lobby.WordHints[randomIndex].Show = true
									triggerWordHintUpdate(lobby)
									break
								}
							}
						}
					}
				}
				if lobby.TimeLeft == 0 {
					go endRound(lobby)
				}
			case <-lobby.timeLeftTickerReset:
				return
			}
		}
	}()

	for _, a := range lobby.Players {
		playersThatAreHigher := 0
		for _, b := range lobby.Players {
			if b.Score > a.Score {
				playersThatAreHigher++
			}
		}

		a.Rank = playersThatAreHigher + 1
	}

	triggerNextTurn(lobby)
	triggerPlayersUpdate(lobby)
	triggerRoundsUpdate(lobby)
	triggerWordHintUpdate(lobby)
}

func selectNextDrawer(lobby *Lobby) {
	for playerIndex, otherPlayer := range lobby.Players {
		if otherPlayer == lobby.Drawer {
			lobby.Drawer = lobby.Players[playerIndex+1]
			return
		}
	}

	for _, otherPlayer := range lobby.Players {
		if otherPlayer == lobby.Drawer {
			return
		}

		if otherPlayer.State == Disconnected {
			continue
		}

		lobby.Drawer = otherPlayer
	}
}

func createWordHintFor(word string) []*WordHint {
	wordHints := make([]*WordHint, 0, len(word))
	for _, char := range word {
		irrelevantChar := char == ' ' || char == '_' || char == '-'
		wordHints = append(wordHints, &WordHint{
			Character: string(char),
			Show:      irrelevantChar,
			Underline: !irrelevantChar,
		})
	}

	return wordHints
}

func showAllInWordHints(hints []*WordHint) []*WordHint {
	newHints := make([]*WordHint, len(hints), len(hints))
	for index, hint := range hints {
		newHints[index] = &WordHint{
			Character: hint.Character,
			Show:      true,
			Underline: hint.Underline,
		}
	}

	return newHints
}

func triggerClearingDrawingBoards(lobby *Lobby) {
	triggerSimpleUpdateEvent("clear-drawing-board", lobby)
}

func triggerNextTurn(lobby *Lobby) {
	triggerSimpleUpdateEvent("next-turn", lobby)
}

func triggerPlayersUpdate(lobby *Lobby) {
	triggerSimpleUpdateEvent("update-players", lobby)
}

func triggerCorrectGuessEvent(lobby *Lobby) {
	triggerSimpleUpdateEvent("correct-guess", lobby)
}

func triggerWordHintUpdate(lobby *Lobby) {
	triggerSimpleUpdateEvent("update-wordhint", lobby)
}

func triggerRoundsUpdate(lobby *Lobby) {
	triggerSimpleUpdateEvent("update-rounds", lobby)
}

func triggerTimeLeftUpdate(lobby *Lobby) {
	event := &JSEvent{Type: "update-time", Data: lobby.TimeLeft}
	for _, otherPlayer := range lobby.Players {
		if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
			otherPlayer.ws.WriteJSON(event)
		}
	}
}

func triggerSimpleUpdateEvent(eventType string, lobby *Lobby) {
	event := &JSEvent{Type: eventType}
	for _, otherPlayer := range lobby.Players {
		if otherPlayer.State != Disconnected && otherPlayer.ws != nil {
			otherPlayer.ws.WriteJSON(event)
		}
	}
}

// LobbyPageData is the data necessary for initially displaying all data of
// the lobbies webpage.
type LobbyPageData struct {
	Players   []*Player
	Port      int
	LobbyID   string
	WordHints []*WordHint
	Round     int
	Rounds    int
}

func getLobby(w http.ResponseWriter, r *http.Request) *Lobby {
	lobbyID := r.URL.Query().Get("id")
	if lobbyID == "" {
		errorPage.ExecuteTemplate(w, "error.html", "The entered URL is incorrect.")
		return nil
	}

	lobby := GetLobby(lobbyID)

	if lobby == nil {
		errorPage.ExecuteTemplate(w, "error.html", "The lobby does not exist.")
	}

	return lobby
}

// GetPlayers returns divs for all players in the lobby to the calling client.
func GetPlayers(w http.ResponseWriter, r *http.Request) {
	lobby := getLobby(w, r)
	if lobby != nil {
		templatingError := lobbyPage.ExecuteTemplate(w, "players", lobby.Players)
		if templatingError != nil {
			errorPage.ExecuteTemplate(w, "error.html", templatingError.Error())
		}
	}
}

// GetWordHint returns the html structure and data for the current word hint.
func GetWordHint(w http.ResponseWriter, r *http.Request) {
	lobby := getLobby(w, r)
	if lobby != nil {
		sessionCookie, noCookieError := r.Cookie("usersession")
		if noCookieError != nil {
			errorPage.ExecuteTemplate(w, "error.html", "You aren't part of this lobby.")
			return
		}

		player := lobby.GetPlayer(sessionCookie.Value)
		if player == nil {
			errorPage.ExecuteTemplate(w, "error.html", "You aren't part of this lobby.")
			return
		}

		var wordHints []*WordHint
		if player.State == Drawing || player.State == Standby {
			wordHints = lobby.WordHintsShown
		} else {
			wordHints = lobby.WordHints
		}

		templatingError := lobbyPage.ExecuteTemplate(w, "word", wordHints)
		if templatingError != nil {
			errorPage.ExecuteTemplate(w, "error.html", templatingError.Error())
		}
	}
}

//GetRounds returns the html structure and data for the current round info.
func GetRounds(w http.ResponseWriter, r *http.Request) {
	lobby := getLobby(w, r)
	if lobby != nil {
		fmt.Fprintf(w, "Round %d of %d", lobby.Round, lobby.Rounds)
	}
}

// CreateLobby allows creating a lobby, optionally returning errors that
// occured during creation.
func CreateLobby(w http.ResponseWriter, r *http.Request) {
	formParseError := r.ParseForm()
	if formParseError != nil {
		panic(formParseError)
	}

	password, passwordInvalid := parsePassword(r.Form.Get("lobby_password"))
	drawingTime, drawingTimeInvalid := parseDrawingTime(r.Form.Get("drawing_time"))
	rounds, roundsInvalid := parseRounds(r.Form.Get("rounds"))
	maxPlayers, maxPlayersInvalid := parseMaxPlayers(r.Form.Get("max_players"))
	customWords, customWordsInvalid := parseCustomWords(r.Form.Get("custom_words"))
	customWordChance, customWordChanceInvalid := parseCustomWordsChance(r.Form.Get("custom_words_chance"))

	//Prevent resetting the form, since that would be annoying as hell.
	pageData := CreatePageData{
		SettingBounds:     lobbySettingBounds,
		Password:          r.Form.Get("lobby_password"),
		DrawingTime:       r.Form.Get("drawing_time"),
		Rounds:            r.Form.Get("rounds"),
		MaxPlayers:        r.Form.Get("max_players"),
		CustomWords:       r.Form.Get("custom_words"),
		CustomWordsChance: r.Form.Get("custom_words_chance"),
	}

	if passwordInvalid != nil {
		pageData.Errors = append(pageData.Errors, passwordInvalid.Error())
	}
	if drawingTimeInvalid != nil {
		pageData.Errors = append(pageData.Errors, drawingTimeInvalid.Error())
	}
	if roundsInvalid != nil {
		pageData.Errors = append(pageData.Errors, roundsInvalid.Error())
	}
	if maxPlayersInvalid != nil {
		pageData.Errors = append(pageData.Errors, maxPlayersInvalid.Error())
	}
	if customWordsInvalid != nil {
		pageData.Errors = append(pageData.Errors, customWordsInvalid.Error())
	}
	if customWordChanceInvalid != nil {
		pageData.Errors = append(pageData.Errors, customWordChanceInvalid.Error())
	}

	if len(pageData.Errors) != 0 {
		err := lobbyCreatePage.ExecuteTemplate(w, "lobby_create.html", pageData)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	} else {
		lobby := createLobby(password, drawingTime, rounds, maxPlayers, customWords, customWordChance)

		var playerName string
		usernameCookie, noCookieError := r.Cookie("username")
		if noCookieError == nil {
			playerName = usernameCookie.Value
		} else {
			playerName = generatePlayerName()
		}

		player := createPlayer(playerName)

		//FIXME Make a dedicated method that uses a mutex?
		lobby.Players = append(lobby.Players, player)
		lobby.Owner = player

		// Use the players generated usersession and pass it as a cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     "usersession",
			Value:    player.UserSession,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
		})

		http.Redirect(w, r, "/lobby?id="+lobby.ID, http.StatusFound)
	}
}

func generatePlayerName() string {
	adjective := strings.Title(petname.Adjective())
	adverb := strings.Title(petname.Adverb())
	name := strings.Title(petname.Name())
	return adverb + adjective + name
}

// JSEvent contains an eventtype and optionally any data.
type JSEvent struct {
	Type string
	Data interface{}
}

// Message represents a message in the chatroom.
type Message struct {
	// Author is the player / thing that wrote the message
	Author string
	// Content is the actual message text.
	Content string
}

// ShowLobby opens a lobby, either opening it directly or asking for a lobby
// password.
func ShowLobby(w http.ResponseWriter, r *http.Request) {
	lobby := getLobby(w, r)
	if lobby != nil {
		// TODO Improve this. Return metadata or so instead.
		userAgent := strings.ToLower(r.UserAgent())
		if !(strings.Contains(userAgent, "gecko") || strings.Contains(userAgent, "chrom") || strings.Contains(userAgent, "opera") || strings.Contains(userAgent, "safari")) {
			errorPage.ExecuteTemplate(w, "error.html", "Sorry, no robots allowed.")
			return
		}

		sessionCookie, noCookieError := r.Cookie("usersession")
		var player *Player
		if noCookieError == nil {
			player = lobby.GetPlayer(sessionCookie.Value)
		}

		if player == nil {
			if len(lobby.Players) >= lobby.MaxPlayers {
				errorPage.ExecuteTemplate(w, "error.html", "Sorry, but the lobby is full.")
				return
			}

			var playerName string
			usernameCookie, noCookieError := r.Cookie("username")
			if noCookieError == nil {
				playerName = usernameCookie.Value
			} else {
				playerName = generatePlayerName()
			}

			player := createPlayer(playerName)

			//FIXME Make a dedicated method that uses a mutex?
			lobby.Players = append(lobby.Players, player)

			pageData := &LobbyPageData{
				Port:    *portHTTP,
				Players: lobby.Players,
				LobbyID: lobby.ID,
				Round:   lobby.Round,
				Rounds:  lobby.Rounds,
			}

			for _, player := range lobby.Players {
				if player.ws != nil {
					player.ws.WriteJSON(JSEvent{Type: "update-players"})
				}
			}

			// Use the players generated usersession and pass it as a cookie.
			http.SetCookie(w, &http.Cookie{
				Name:     "usersession",
				Value:    player.UserSession,
				Path:     "/",
				SameSite: http.SameSiteStrictMode,
			})

			lobbyPage.ExecuteTemplate(w, "lobby.html", pageData)
		} else {
			pageData := &LobbyPageData{
				Port:    *portHTTP,
				Players: lobby.Players,
				LobbyID: lobby.ID,
				Round:   lobby.Round,
				Rounds:  lobby.Rounds,
			}

			lobbyPage.ExecuteTemplate(w, "lobby.html", pageData)
		}
	}
}

// WordHint describes a character of the word that is to be guessed, whether
// the character should be shown and whether it should be underlined on the
// UI.
type WordHint struct {
	Character string
	Show      bool
	Underline bool
}

func parsePlayerName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return trimmed, errors.New("the player name must not be empty")
	}

	return trimmed, nil
}

func parsePassword(value string) (string, error) {
	return value, nil
}

func parseDrawingTime(value string) (int, error) {
	result, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, errors.New("the drawing time must be numeric")
	}

	if result < lobbySettingBounds.MinDrawingTime {
		return 0, fmt.Errorf("drawing time must not be smaller than %d", lobbySettingBounds.MinDrawingTime)
	}

	if result > lobbySettingBounds.MaxDrawingTime {
		return 0, fmt.Errorf("drawing time must not be greater than %d", lobbySettingBounds.MaxDrawingTime)
	}

	return int(result), nil
}

func parseRounds(value string) (int, error) {
	result, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, errors.New("the rounds amount must be numeric")
	}

	if result < lobbySettingBounds.MinRounds {
		return 0, fmt.Errorf("rounds must not be smaller than %d", lobbySettingBounds.MinRounds)
	}

	if result > lobbySettingBounds.MaxRounds {
		return 0, fmt.Errorf("rounds must not be greater than %d", lobbySettingBounds.MaxRounds)
	}

	return int(result), nil
}

func parseMaxPlayers(value string) (int, error) {
	result, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, errors.New("the max players amount must be numeric")
	}

	if result < lobbySettingBounds.MinMaxPlayers {
		return 0, fmt.Errorf("maximum players must not be smaller than %d", lobbySettingBounds.MinMaxPlayers)
	}

	if result > lobbySettingBounds.MaxMaxPlayers {
		return 0, fmt.Errorf("maximum players must not be greater than %d", lobbySettingBounds.MaxMaxPlayers)
	}

	return int(result), nil
}

func parseCustomWords(value string) ([]string, error) {
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return nil, nil
	}

	result := strings.Split(trimmedValue, ",")
	for index, item := range result {
		trimmedItem := strings.ToLower(strings.TrimSpace(item))
		if trimmedItem == "" {
			return nil, errors.New("custom words must not be empty")
		}
		result[index] = trimmedItem
	}

	return result, nil
}

func parseCustomWordsChance(value string) (int, error) {
	result, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, errors.New("the custom word chance must be numeric")
	}

	if result < 0 {
		return 0, errors.New("custom word chance must not be lower than 0")
	}

	if result > 100 {
		return 0, errors.New("custom word chance must not be higher than 100")
	}

	return int(result), nil
}
