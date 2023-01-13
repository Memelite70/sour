package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cfoust/sour/pkg/game"
	"github.com/cfoust/sour/svc/cluster/ingress"
	"github.com/cfoust/sour/svc/cluster/servers"

	"github.com/repeale/fp-go/option"
	"github.com/rs/zerolog/log"
)

func (server *Cluster) GivePrivateMatchHelp(ctx context.Context, user *User, gameServer *servers.GameServer) {
	tick := time.NewTicker(30 * time.Second)

	message := fmt.Sprintf("This is your private server. Have other players join by saying '#join %s' in any Sour server.", gameServer.Id)

	if user.Connection.Type() == ingress.ClientTypeWS {
		message = fmt.Sprintf("This is your private server. Have other players join by saying '#join %s' in any Sour server or by sending the link in your URL bar. (We also copied it for you!)", gameServer.Id)
	}

	sessionContext := user.ServerSessionContext()

	for {
		gameServer.Mutex.Lock()
		numClients := gameServer.NumClients
		gameServer.Mutex.Unlock()

		if numClients < 2 {
			user.SendServerMessage(message)
		} else {
			return
		}

		select {
		case <-sessionContext.Done():
			return
		case <-tick.C:
			continue
		case <-ctx.Done():
			return
		}
	}
}

func getModeNames() []string {
	return []string{
		"ffa", "coop", "teamplay", "insta", "instateam", "effic", "efficteam", "tac", "tacteam", "capture", "regencapture", "ctf", "instactf", "protect", "instaprotect", "hold", "instahold", "efficctf", "efficprotect", "effichold", "collect", "instacollect", "efficcollect",
	}
}

func getModeNumber(mode string) opt.Option[int] {
	for i, name := range getModeNames() {
		if name == mode {
			return opt.Some(i)
		}
	}

	return opt.None[int]()
}

type CreateParams struct {
	Map    opt.Option[string]
	Preset opt.Option[string]
	Mode   opt.Option[int]
}

func (server *Cluster) inferCreateParams(args []string) (*CreateParams, error) {
	params := CreateParams{}

	for _, arg := range args {
		mode := getModeNumber(arg)
		if opt.IsSome(mode) {
			params.Mode = mode
			continue
		}

		map_ := server.manager.Maps.FindMap(arg)
		if opt.IsSome(map_) {
			params.Map = opt.Some(arg)
			continue
		}

		preset := server.manager.FindPreset(arg, false)
		if opt.IsSome(preset) {
			params.Preset = opt.Some(preset.Value.Name)
			continue
		}

		return nil, fmt.Errorf("argument '%s' neither corresponded to a map nor a game mode", arg)
	}

	return &params, nil
}

func (server *Cluster) RunCommand(ctx context.Context, command string, user *User) (handled bool, response string, err error) {
	logger := user.Logger().With().Str("command", command).Logger()
	logger.Info().Msg("running command")

	args := strings.Split(command, " ")

	if len(args) == 0 {
		return false, "", errors.New("invalid command")
	}

	switch args[0] {
	case "creategame":
		params := &CreateParams{}
		if len(args) > 1 {
			params, err = server.inferCreateParams(args[1:])
			if err != nil {
				return true, "", err
			}
		}

		server.createMutex.Lock()
		defer server.createMutex.Unlock()

		lastCreate, hasLastCreate := server.lastCreate[user.Connection.Host()]
		if hasLastCreate && (time.Now().Sub(lastCreate)) < CREATE_SERVER_COOLDOWN {
			return true, "", errors.New("too soon since last server create")
		}

		existingServer, hasExistingServer := server.hostServers[user.Connection.Host()]
		if hasExistingServer {
			server.manager.RemoveServer(existingServer)
		}

		logger.Info().Msg("starting server")

		presetName := ""
		if opt.IsSome(params.Preset) {
			presetName = params.Preset.Value
		}

		gameServer, err := server.manager.NewServer(server.serverCtx, presetName, false)
		if err != nil {
			logger.Error().Err(err).Msg("failed to create server")
			return true, "", errors.New("failed to create server")
		}

		logger = logger.With().Str("server", gameServer.Reference()).Logger()

		err = gameServer.StartAndWait(server.serverCtx)
		if err != nil {
			logger.Error().Err(err).Msg("server failed to start")
			return true, "", errors.New("server failed to start")
		}

		if opt.IsSome(params.Mode) && opt.IsSome(params.Map) {
			gameServer.SendCommand(fmt.Sprintf("changemap %s %d", params.Map.Value, params.Mode.Value))
		} else if opt.IsSome(params.Mode) {
			gameServer.SendCommand(fmt.Sprintf("setmode %d", params.Mode.Value))
		} else if opt.IsSome(params.Map) {
			gameServer.SendCommand(fmt.Sprintf("setmap %s", params.Map.Value))
		}

		server.lastCreate[user.Connection.Host()] = time.Now()
		server.hostServers[user.Connection.Host()] = gameServer

		connected, err := user.ConnectToServer(gameServer, "", false, true)
		go server.GivePrivateMatchHelp(server.serverCtx, user, user.Server)

		go func() {
			ctx, cancel := context.WithTimeout(user.Connection.SessionContext(), time.Second*10)
			defer cancel()

			select {
			case status := <-connected:
				if !status {
					return
				}

				gameServer.SendCommand(fmt.Sprintf("grantmaster %d", user.GetClientNum()))
			case <-ctx.Done():
				log.Info().Msgf("context finished")
				return
			}
		}()

		return true, "", nil

	case "edit":
		isOwner, err := user.IsOwner(ctx)
		if err != nil {
			return true, "", err
		}

		if !isOwner {
			return true, "", fmt.Errorf("this is not your space")
		}

		space := user.GetSpace()
		editing := space.Editing
		current := editing.IsOpenEdit()
		editing.SetOpenEdit(!current)

		canEdit := editing.IsOpenEdit()

		if canEdit {
			server.AnnounceInServer(ctx, space.Server, "editing is now enabled")
		} else {
			server.AnnounceInServer(ctx, space.Server, "editing is now disabled")
		}

		return true, "", nil

	case "join":
		if len(args) != 2 {
			return true, "", errors.New("join takes a single argument")
		}

		target := args[1]

		user.Mutex.Lock()
		if user.Server != nil && user.Server.IsReference(target) {
			logger.Info().Msg("user already connected to target")
			user.Mutex.Unlock()
			break
		}
		user.Mutex.Unlock()

		for _, gameServer := range server.manager.Servers {
			if !gameServer.IsReference(target) || !gameServer.IsRunning() {
				continue
			}

			_, err := user.Connect(gameServer)
			if err != nil {
				return true, "", err
			}

			return true, "", nil
		}

		// Look for a space
		space, err := server.spaces.SearchSpace(ctx, target)
		if err != nil {
			return true, "", err
		}

		if space != nil {
			instance, err := server.spaces.StartSpace(ctx, target)
			if err != nil {
				return true, "", err
			}
			_, err = user.ConnectToSpace(instance.Server, instance.Space.GetID())
			return true, "", err
		}

		logger.Warn().Msgf("could not find server: %s", target)
		return true, "", fmt.Errorf("failed to find server or space matching %s", target)

	case "duel":
		duelType := ""
		if len(args) > 1 {
			duelType = args[1]
		}

		err := server.matches.Queue(user, duelType)
		if err != nil {
			// Theoretically, there might also just not be a default, but whatever.
			return true, "", fmt.Errorf("duel type '%s' does not exist", duelType)
		}

		return true, "", nil

	case "stopduel":
		server.matches.Dequeue(user)
		return true, "", nil

	case "home":
		server.GoHome(server.serverCtx, user)
		return true, "", nil

	case "help":
		messages := []string{
			fmt.Sprintf("%s: create a private game", game.Blue("#creategame")),
			fmt.Sprintf("%s: join a Sour game server by room code", game.Blue("#join [code]")),
			fmt.Sprintf("%s: queue for a duel", game.Blue("#duel")),
			fmt.Sprintf("%s: leave the duel queue", game.Blue("#stopduel")),
		}

		for _, message := range messages {
			user.SendServerMessage(message)
		}

		return true, "", nil
	}

	return false, "", nil
}

func (server *Cluster) RunCommandWithTimeout(ctx context.Context, command string, user *User) (handled bool, response string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)

	resultChannel := make(chan ingress.CommandResult)

	defer cancel()

	go func() {
		handled, response, err := server.RunCommand(ctx, command, user)
		resultChannel <- ingress.CommandResult{
			Handled:  handled,
			Err:      err,
			Response: response,
		}
	}()

	select {
	case result := <-resultChannel:
		return result.Handled, result.Response, result.Err
	case <-ctx.Done():
		cancel()
		return false, "", errors.New("command timed out")
	}

}
