/*
package apisrv provides an implementation of the gRPC server defined in
../../../api/protobuf-spec/backend.proto

Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package apisrv

import (
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/open-match/config"
	"github.com/GoogleCloudPlatform/open-match/internal/expbo"
	"github.com/GoogleCloudPlatform/open-match/internal/pb"
	"github.com/GoogleCloudPlatform/open-match/internal/serving"
	redishelpers "github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/ignorelist"
	"github.com/GoogleCloudPlatform/open-match/internal/statestorage/redis/redispb"
	"github.com/cenkalti/backoff"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/tidwall/gjson"

	"github.com/gomodule/redigo/redis"
	"github.com/rs/xid"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// backendAPI implements backend API Server, the server generated by compiling
// the protobuf, by fulfilling the API Client interface.
type backendAPI struct {
	cfg    config.View
	pool   *redis.Pool
	logger *log.Entry
}

// Bind binds the gRPC endpoint to OpenMatchServer
func Bind(omSrv *serving.OpenMatchServer) {
	handler := &backendAPI{
		cfg:    omSrv.Config,
		pool:   omSrv.RedisPool,
		logger: omSrv.Logger,
	}
	omSrv.GrpcServer.AddService(func(server *grpc.Server) {
		pb.RegisterBackendServer(server, handler)
	})
}

// CreateMatch is this service's implementation of the CreateMatch gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) CreateMatch(c context.Context, req *pb.CreateMatchRequest) (*pb.CreateMatchResponse, error) {
	profile := proto.Clone(req.Match).(*pb.MatchObject)
	beLog := s.logger

	// Get a cancel-able context
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateMatch"

	// Generate a request to fill the profile. Make a unique request ID.
	moID := xid.New().String()
	requestKey := moID + "." + profile.Id

	/*
		// Debugging logs
		beLog.Info("Pools nil? ", (profile.Pools == nil))
		beLog.Info("Pools empty? ", (len(profile.Pools) == 0))
		beLog.Info("Rosters nil? ", (profile.Rosters == nil))
		beLog.Info("Rosters empty? ", (len(profile.Rosters) == 0))
		beLog.Info("config set for json.pools?", s.cfg.IsSet("jsonkeys.pools"))
		beLog.Info("contents key?", s.cfg.GetString("jsonkeys.pools"))
		beLog.Info("contents exist?", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).Exists())
	*/

	// Case where no protobuf pools was passed; check if there's a JSON version in the properties.
	// This is for backwards compatibility, it is recommended you populate the protobuf's
	// 'pools' field directly and pass it to CreateMatch/ListMatches
	if profile.Pools == nil && s.cfg.IsSet("jsonkeys.pools") &&
		gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).Exists() {
		poolsJSON := fmt.Sprintf("{\"pools\": %v}", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.pools")).String())
		ppLog := beLog.WithFields(log.Fields{"jsonkey": s.cfg.GetString("jsonkeys.pools")})
		ppLog.Info("poolsJSON: ", poolsJSON)

		ppools := &pb.MatchObject{}
		err := jsonpb.UnmarshalString(poolsJSON, ppools)
		if err != nil {
			ppLog.Error("failed to parse JSON to protobuf pools")
		} else {
			profile.Pools = ppools.Pools
			ppLog.Info("parsed JSON to protobuf pools")
		}
	}

	// Case where no protobuf roster was passed; check if there's a JSON version in the properties.
	// This is for backwards compatibility, it is recommended you populate the
	// protobuf's 'rosters' field directly and pass it to CreateMatch/ListMatches
	if profile.Rosters == nil && s.cfg.IsSet("jsonkeys.rosters") &&
		gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.rosters")).Exists() {
		rostersJSON := fmt.Sprintf("{\"rosters\": %v}", gjson.Get(profile.Properties, s.cfg.GetString("jsonkeys.rosters")).String())
		rLog := beLog.WithFields(log.Fields{"jsonkey": s.cfg.GetString("jsonkeys.rosters")})

		prosters := &pb.MatchObject{}
		err := jsonpb.UnmarshalString(rostersJSON, prosters)
		if err != nil {
			rLog.Error("failed to parse JSON to protobuf rosters")
		} else {
			profile.Rosters = prosters.Rosters
			rLog.Info("parsed JSON to protobuf rosters")
		}
	}

	// Add fields for all subsequent logging
	beLog = beLog.WithFields(log.Fields{
		"profileID":     profile.Id,
		"func":          funcName,
		"matchObjectID": moID,
		"requestKey":    requestKey,
	})
	beLog.Info("gRPC call executing")
	beLog.Info("profile is")
	beLog.Info(profile)

	// Write profile to state storage
	err := redispb.MarshalToRedis(ctx, s.pool, profile, s.cfg.GetInt("redis.expirations.matchobject"))
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage failure to create match profile")

		// Failure! Return empty match object and the error
		return nil, status.Error(codes.Unknown, err.Error())
	}
	beLog.Info("Profile written to state storage")

	// Queue the request ID to be sent to an MMF
	_, err = redishelpers.Update(ctx, s.pool, s.cfg.GetString("queues.profiles.name"), requestKey)
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage failure to queue profile")

		// Failure! Return empty match object and the error
		return nil, status.Error(codes.Unknown, err.Error())
	}
	beLog.Info("Profile added to processing queue")

	watcherBO := backoff.NewExponentialBackOff()
	if err := expbo.UnmarshalExponentialBackOff(s.cfg.GetString("api.backend.backoff"), watcherBO); err != nil {
		beLog.WithError(err).Warn("Could not parse backoff string, using default backoff parameters for MatchObject watcher")
	}

	watcherBOCtx := backoff.WithContext(watcherBO, ctx)

	// get and return matchobject, it will be written to the requestKey when the MMF has finished.
	watchChan := redispb.Watcher(watcherBOCtx, s.pool, pb.MatchObject{Id: requestKey}) // Watcher() runs the appropriate Redis commands.
	newMO, ok := <-watchChan
	if !ok {
		// ok is false if watchChan has been closed by redispb.Watcher()
		// This happens when Watcher stops because of context cancellation or backing off reached time limit
		if watcherBOCtx.Context().Err() != nil {
			newMO.Error = "channel closed: " + watcherBOCtx.Context().Err().Error()
		} else {
			newMO.Error = "channel closed: backoff deadline exceeded"
		}
		return nil, status.Errorf(codes.Unavailable, "Error retrieving matchmaking results from state storage: %s", newMO.Error)
	}

	// 'ok' was true, so properties should contain the results from redis.
	// Do basic error checking on the returned JSON
	if !gjson.Valid(profile.Properties) {
		newMO.Error = "retreived properties json was malformed"
	}

	// TODO test that this is the correct condition for an empty error.
	if newMO.Error != "" {
		return nil, status.Error(codes.Unknown, newMO.Error)
	}

	beLog.Info("Matchmaking results received, returning to backend client")
	return &pb.CreateMatchResponse{
		Match: &newMO,
	}, nil
}

// ListMatches is this service's implementation of the ListMatches gRPC method
// defined in api/protobuf-spec/backend.proto
// This is the streaming version of CreateMatch - continually submitting the
// profile to be filled until the requesting service ends the connection.
func (s *backendAPI) ListMatches(req *pb.ListMatchesRequest, matchStream pb.Backend_ListMatchesServer) error {
	p := proto.Clone(req.Match).(*pb.MatchObject)
	beLog := s.logger

	// call creatematch in infinite loop as long as the stream is open
	ctx := matchStream.Context() // https://talks.golang.org/2015/gotham-grpc.slide#30

	// Create context for tagging OpenCensus metrics.
	funcName := "ListMatches"

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"profileID": p.Id,
	}).Info("gRPC call executing. Calling CreateMatch. Looping until cancelled.")

	for {
		select {
		case <-ctx.Done():
			// Context cancelled, probably because the client cancelled their request, time to exit.
			beLog.WithFields(log.Fields{
				"profileID": p.Id,
			}).Info("gRPC Context cancelled; client is probably finished receiving matches")

			// TODO: need to make sure that in-flight matches don't get leaked here.
			return nil

		default:
			// Retreive results from Redis
			requestProfile := proto.Clone(p).(*pb.MatchObject)
			/*
				beLog.Debug("new profile requested!")
				beLog.Debug(requestProfile)
				beLog.Debug(&requestProfile)
			*/
			mo, err := s.CreateMatch(ctx, &pb.CreateMatchRequest{
				Match: requestProfile,
			})

			beLog = beLog.WithFields(log.Fields{"func": funcName})

			if err != nil {
				beLog.WithFields(log.Fields{"error": err.Error()}).Error("Failure calling CreateMatch")
				return status.Error(codes.Unavailable, err.Error())
			}
			beLog.WithFields(log.Fields{"matchProperties": fmt.Sprintf("%v", mo)}).Debug("Streaming back match object")
			res := proto.Clone(mo.Match).(*pb.MatchObject)
			matchStream.Send(&pb.ListMatchesResponse{
				Match: res,
			})

			// TODO: This should be tunable, but there should be SOME sleep here, to give a requestor a window
			// to cleanly close the connection after receiving a match object when they know they don't want to
			// request any more matches.
			time.Sleep(2 * time.Second)
		}
	}
}

// DeleteMatch is this service's implementation of the DeleteMatch gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) DeleteMatch(ctx context.Context, req *pb.DeleteMatchRequest) (*pb.DeleteMatchResponse, error) {
	beLog := s.logger

	// Create context for tagging OpenCensus metrics.
	funcName := "DeleteMatch"

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"matchObjectID": req.Match.Id,
	}).Info("gRPC call executing")

	err := redishelpers.Delete(ctx, s.pool, req.Match.Id)
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		return nil, status.Error(codes.Unknown, err.Error())
	}

	beLog.WithFields(log.Fields{
		"matchObjectID": req.Match.Id,
	}).Info("Match Object deleted.")
	return &pb.DeleteMatchResponse{}, nil
}

// CreateAssignments is this service's implementation of the CreateAssignments gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) CreateAssignments(ctx context.Context, req *pb.CreateAssignmentsRequest) (*pb.CreateAssignmentsResponse, error) {
	a := proto.Clone(req.Assignment).(*pb.Assignments)
	beLog := s.logger

	// Make a map of players and what assignments we want to send them.
	playerIDs := make([]string, 0)
	players := make(map[string]string, 0)
	for _, roster := range a.Rosters { // Loop through all rosters
		for _, player := range roster.Players { // Loop through all players in this roster
			if player.Id != "" {
				if player.Assignment == "" {
					// No player-specific assignment, so use the default one in
					// the Assignment message.
					player.Assignment = a.Assignment
				}
				players[player.Id] = player.Assignment
				beLog.Debug(fmt.Sprintf("playerid %v assignment %v", player.Id, player.Assignment))
			}
		}
		playerIDs = append(playerIDs, getPlayerIdsFromRoster(roster)...)
	}

	// Create context for tagging OpenCensus metrics.
	funcName := "CreateAssignments"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"numAssignments": len(players),
	}).Info("gRPC call executing")

	// TODO: These two calls are done in two different transactions; could be
	// combined as an optimization but probably not particularly necessary
	// Send the players their assignments.
	err := redishelpers.UpdateMultiFields(ctx, s.pool, players, "assignment")

	// Move these players from the proposed list to the deindexed list.
	ignorelist.Move(ctx, s.pool, playerIDs, "proposed", "deindexed")

	// Issue encountered
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		stats.Record(fnCtx, BeAssignmentFailures.M(int64(len(players))))
		return nil, status.Error(codes.Unknown, err.Error())
	}

	// Success!
	beLog.WithFields(log.Fields{
		"numPlayers": len(players),
	}).Info("Assignments complete")

	stats.Record(fnCtx, BeAssignments.M(int64(len(players))))
	return &pb.CreateAssignmentsResponse{}, nil
}

// DeleteAssignments is this service's implementation of the DeleteAssignments gRPC method
// defined in api/protobuf-spec/backend.proto
func (s *backendAPI) DeleteAssignments(ctx context.Context, req *pb.DeleteAssignmentsRequest) (*pb.DeleteAssignmentsResponse, error) {
	assignments := getPlayerIdsFromRoster(req.Roster)
	beLog := s.logger

	// Create context for tagging OpenCensus metrics.
	funcName := "DeleteAssignments"
	fnCtx, _ := tag.New(ctx, tag.Insert(KeyMethod, funcName))

	beLog = beLog.WithFields(log.Fields{"func": funcName})
	beLog.WithFields(log.Fields{
		"numAssignments": len(assignments),
	}).Info("gRPC call executing")

	err := redishelpers.DeleteMultiFields(ctx, s.pool, assignments, "assignment")

	// Issue encountered
	if err != nil {
		beLog.WithFields(log.Fields{
			"error":     err.Error(),
			"component": "statestorage",
		}).Error("State storage error")

		stats.Record(fnCtx, BeAssignmentDeletionFailures.M(int64(len(assignments))))
		return nil, status.Error(codes.Unknown, err.Error())
	}

	// Success!
	stats.Record(fnCtx, BeAssignmentDeletions.M(int64(len(assignments))))
	return &pb.DeleteAssignmentsResponse{}, nil
}

// getPlayerIdsFromRoster returns the slice of player ID strings contained in
// the input roster.
func getPlayerIdsFromRoster(r *pb.Roster) []string {
	playerIDs := make([]string, 0)
	for _, p := range r.Players {
		playerIDs = append(playerIDs, p.Id)
	}
	return playerIDs
}
