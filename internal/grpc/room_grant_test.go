package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter/mesh"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

type grantingAdapter struct {
	mesh.Adapter
	grants map[string]mesh.RoomGrant
}

func (g *grantingAdapter) RoomGrant(conversationID string) (mesh.RoomGrant, bool) {
	rg, ok := g.grants[conversationID]
	return rg, ok
}

func TestGetRoomGrantReturnsTheMeshConversationsCurrentGrant(t *testing.T) {
	server := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)
	expires := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	server.adapters[mesh.Platform] = &grantingAdapter{grants: map[string]mesh.RoomGrant{
		"t_1": {RoomID: "research", Grant: "g.r.ant", ExpiresAt: expires, APIURL: "https://astro.test"},
	}}

	got, err := server.GetRoomGrant(context.Background(), &pb.RoomGrantRequest{ConversationId: "t_1"})
	if err != nil {
		t.Fatalf("get room grant: %v", err)
	}
	if !got.GetFound() || got.GetRoomId() != "research" || got.GetGrant() != "g.r.ant" || got.GetApiUrl() != "https://astro.test" || !got.GetExpiresAt().AsTime().Equal(expires) {
		t.Errorf("room grant = %+v, want the mesh adapter's grant for research", got)
	}

	missing, err := server.GetRoomGrant(context.Background(), &pb.RoomGrantRequest{ConversationId: "web-conversation"})
	if err != nil || missing.GetFound() {
		t.Errorf("room grant for a non-mesh conversation = %+v, %v; want found=false", missing, err)
	}
}
