package main

import (
	"bytes"
	"bedrock-cross-version-relay/internal/blocktranslate"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	legacy "github.com/shawtymarco/go-multiversion/protocols/v1_26_30"
)

// predResizedLog logs the movement-prediction bitset resize exactly once.
var predResizedLog atomic.Bool

// ConvertFromLatest runs when WRITING a packet down the 1001 host conn (client → host direction).
//
// ★ `ClientMovementPredictionSync.ActorFlags` sent by a 2193 client carries a bitset sized
// `EntityDataFlagCount` for 2193 (=131); the 1001 adapter demands 130 ⇒ `Writer.Bitset` panics
// with "bitset size mismatch: expected 130, got 131" ⇒ the relay DROPS the packet (log 20-26-31:
// 8 times in ~2 minutes) ⇒ the host receives no movement prediction ⇒ the host rolls the position
// back ⇒ the player is dragged backwards while running.
// Note: this must live HERE (host1001Protocol) to take effect — this packet travels C→H and never
// passes through the client conn's clientVersionProtocol.
func (p host1001Protocol) ConvertFromLatest(pk packet.Packet, conn *minecraft.Conn) []packet.Packet {
	if sync, ok := pk.(*packet.ClientMovementPredictionSync); ok {
		const hostActorFlagCount = 130
		if sync.ActorFlags.Len() != hostActorFlagCount {
			cloned := *sync
			cloned.ActorFlags = resizeBitset(sync.ActorFlags, hostActorFlagCount)
			if predResizedLog.CompareAndSwap(false, true) {
				slog.Info("xvrelay: shrinking the ClientMovementPredictionSync bitset for the 1001 host",
					"from", sync.ActorFlags.Len(), "to", hostActorFlagCount)
			}
			return []packet.Packet{&cloned}
		}
	}
	return p.Protocol.ConvertFromLatest(pk, conn)
}

// host1001Protocol replaces only the lossy incoming CraftingData decoder.
// The pinned adapter discards numeric ingredient IDs in items.go. By the time
// ConvertToLatest runs they cannot be recovered. All other packets and outgoing
// layouts still use that pinned adapter. Registry lookup is per connection.
type host1001Protocol struct{ minecraft.Protocol }

func (p host1001Protocol) Packets(listener bool) packet.Pool {
	pool := p.Protocol.Packets(listener)
	if !listener {
		pool[packet.IDCraftingData] = func() packet.Packet { return &packet.Unknown{PacketID: packet.IDCraftingData} }
	}
	return pool
}

func (p host1001Protocol) ConvertToLatest(pk packet.Packet, conn *minecraft.Conn) []packet.Packet {
	if raw, ok := pk.(*packet.Unknown); ok && raw.PacketID == packet.IDCraftingData {
		if conn == nil {
			panic(fmt.Errorf("1001 recipes require the host connection registry"))
		}
		items := conn.GameData().Items
		var shieldID int32
		for _, item := range items {
			if item.Name == "minecraft:shield" {
				shieldID = int32(item.RuntimeID)
			}
		}
		decoded, err := decodeHostRecipes1001(raw.Payload, items, shieldID)
		if err != nil {
			slog.Error("xvrelay: host1001 recipe translation failed", "error", err)
			_ = conn.Close()
			panic(err)
		} // gophertunnel converts decode panics to connection errors.
		slog.Info("xvrelay: host1001 recipes decoded with connection registry", "items", len(items),
			"shaped", len(decoded.ShapedRecipes), "shapeless", len(decoded.ShapelessRecipes))
		return []packet.Packet{decoded}
	}
	return p.Protocol.ConvertToLatest(pk, conn)
}

// Only reading is implemented here. Base legacy IO retains its item/NBT and
// material-reducer layouts. Recipe envelopes follow the pinned 1001 schema;
// numeric descriptors are resolved BEFORE their IDs can be discarded.
type hostRecipeReader struct {
	protocol.IO
	items       map[int16]string
	nativeItems map[string]int32
	// skip: the recipe CURRENTLY being read references an item that does not exist in the native
	// registry (a host addon/resource-pack item, e.g. `dl:copper_lantern`). Must NOT panic:
	// one unknown item is not allowed to kill the whole host link.
	//
	// ★ Evidence: log 13:29:22 (world mrzerovn, host1001) —
	//   "decode host1001 CraftingData: recipe item dl:copper_lantern has no native item mapping"
	//   then "read packet: ... link=host" ⇒ host conn closed ⇒ client session died after 3.33 s.
	// nativeItemID used to panic, so ONE addon recipe took down the entire session.
	skip bool
	// unmapped: number of failed ID lookups (logging only, not the number of dropped recipes).
	unmapped int
	// reducerSkip: per-element skip flag for MaterialReducers (FuncSlice does not expose the index,
	// so the flags are tracked alongside).
	reducerSkip []bool
}

// Item reads the legacy stack, then resolves its network ID through names.
// CraftingData's numeric output IDs are not converted by the pinned adapter.
func (r *hostRecipeReader) Item(x *protocol.ItemStack) {
	r.IO.Item(x)
	if x.NetworkID == 0 {
		return
	}
	x.NetworkID = r.nativeItemID(x.NetworkID)
}

// nativeItemID translates a host1001 item ID into the native 2193 ID.
//
// Two DISTINCT cases, do not merge them:
//   - the ID is absent from the HOST's ItemRegistry ⇒ the data really is inconsistent ⇒ still panic
//     (TestHostRecipeFailureClosesConnection / TestLegacyRecipesRejectUnknownNumericIngredient
//     assert this: an unknown ID must never be silently turned into an empty item).
//   - the NAME exists in the host registry but NOT in the native one ⇒ a map addon item
//     (e.g. `dl:copper_lantern`) ⇒ drop just that recipe, do NOT kill the host link.
func (r *hostRecipeReader) nativeItemID(id int32) int32 {
	if id == 0 {
		return 0
	}
	name, ok := r.items[int16(id)]
	if !ok {
		panic(fmt.Errorf("1001 recipe item ID %d is absent from host ItemRegistry", id))
	}
	native, ok := r.nativeItems[name]
	if !ok {
		// Addon/resource-pack item: the client does not have it, so the recipe is unusable.
		// Drop the recipe, keep the rest and keep the host link alive.
		r.skip = true
		r.unmapped++
		return 0
	}
	return native
}

func (r *hostRecipeReader) MaterialReducer(x *protocol.MaterialReducer) {
	r.skip = false
	var packed int32
	r.Varint32(&packed)
	x.InputItem = protocol.ItemType{NetworkID: r.nativeItemID(packed >> 16), MetadataValue: uint32(uint16(packed))}
	protocol.Slice(r, &x.Outputs)
	for i := range x.Outputs {
		x.Outputs[i].NetworkID = r.nativeItemID(x.Outputs[i].NetworkID)
	}
	r.reducerSkip = append(r.reducerSkip, r.skip)
}

func (r *hostRecipeReader) SliceLength(n, max uint32) {
	r.IO.(interface{ SliceLength(uint32, uint32) }).SliceLength(n, max)
}

func (r *hostRecipeReader) ItemDescriptorCount(x *protocol.ItemDescriptorCount) {
	var kind uint8
	r.Uint8(&kind)
	switch kind {
	case 0:
		x.Descriptor = &protocol.InvalidItemDescriptor{}
	case 1:
		var id, meta int16
		r.Int16(&id)
		if id == 0 {
			x.Descriptor = &protocol.InvalidItemDescriptor{}
		} else {
			r.Int16(&meta)
			name, ok := r.items[id]
			if !ok {
				// The ID is absent from the host ItemRegistry ⇒ the data is inconsistent ⇒ fail
				// (see nativeItemID). Addon items still take the branch below because their NAME
				// exists in the host registry.
				panic(fmt.Errorf("1001 recipe ingredient ID %d is absent from host ItemRegistry", id))
			}
			x.Descriptor = &protocol.DefaultItemDescriptor{Name: name, MetadataValue: int32(meta)}
		}
	case 2:
		var expression string
		var version uint8
		r.String(&expression)
		r.Uint8(&version)
		x.Descriptor = &protocol.MoLangItemDescriptor{Expression: expression, Version: int16(version)}
	case 3:
		var tag string
		r.String(&tag)
		x.Descriptor = &protocol.ItemTagItemDescriptor{Tag: tag}
	case 4, 5:
		var name string
		var meta int16
		r.String(&name)
		if kind == 4 {
			r.Int16(&meta)
		}
		x.Descriptor = &protocol.DefaultItemDescriptor{Name: name, MetadataValue: int32(meta)}
	default:
		panic(fmt.Errorf("unknown 1001 recipe descriptor %d", kind))
	}
	r.Varint32(&x.Count)
}

func (r *hostRecipeReader) unlock(x *protocol.Optional[protocol.RecipeUnlockRequirement]) {
	var context uint8
	r.Uint8(&context)
	u := protocol.RecipeUnlockRequirement{Context: int32(context)}
	if context == protocol.RecipeUnlockContextNone {
		protocol.FuncSlice(r, &u.Ingredients, r.ItemDescriptorCount)
	}
	*x = protocol.Option(u)
}

func (r *hostRecipeReader) shapeless(x *protocol.ShapelessRecipe) {
	r.String(&x.RecipeID)
	protocol.FuncSlice(r, &x.Input, r.ItemDescriptorCount)
	protocol.FuncSlice(r, &x.Output, r.Item)
	r.UUID(&x.UUID)
	r.String(&x.Block)
	r.Varint32(&x.Priority)
	r.unlock(&x.UnlockRequirement)
	r.Varuint32(&x.RecipeNetworkID)
}

func (r *hostRecipeReader) shaped(x *protocol.ShapedRecipe) {
	r.String(&x.RecipeID)
	r.Varint32(&x.Width)
	r.Varint32(&x.Height)
	if x.Width < 1 || x.Width > 3 || x.Height < 1 || x.Height > 3 {
		panic(fmt.Errorf("invalid 1001 recipe dimensions %dx%d", x.Width, x.Height))
	}
	protocol.FuncSliceOfLen(r, uint32(x.Width*x.Height), &x.Input, r.ItemDescriptorCount)
	protocol.FuncSlice(r, &x.Output, r.Item)
	r.UUID(&x.UUID)
	r.String(&x.Block)
	r.Varint32(&x.Priority)
	r.Bool(&x.AssumeSymmetry)
	r.unlock(&x.UnlockRequirement)
	r.Varuint32(&x.RecipeNetworkID)
}

func decodeHostRecipes1001(raw []byte, entries []protocol.ItemEntry, shieldID int32) (out *packet.CraftingData, err error) {
	defer func() {
		if v := recover(); v != nil {
			out = nil
			err = fmt.Errorf("decode host1001 CraftingData: %v", v)
		}
	}()
	buf := bytes.NewBuffer(raw)
	native, nativeErr := blocktranslate.Items(2193)
	if nativeErr != nil {
		return nil, nativeErr
	}
	r := &hostRecipeReader{IO: legacy.New().NewReader(buf, shieldID, true), items: make(map[int16]string, len(entries)), nativeItems: make(map[string]int32, len(native))}
	for _, item := range native {
		r.nativeItems[item.Name] = int32(item.RuntimeID)
	}
	for _, item := range entries {
		r.items[item.RuntimeID] = item.Name
	}
	out = &packet.CraftingData{}
	var count uint32
	r.Varuint32(&count)
	r.SliceLength(count, 65536)
	dropped := 0
	for range count {
		var kind int32
		r.Varint32(&kind)
		// Every recipe starts out "translatable"; nativeItemID raises the flag when it meets an
		// addon item that is absent from the native registry.
		r.skip = false
		switch kind {
		case 0, 5, 6:
			var x protocol.ShapelessRecipe
			r.shapeless(&x)
			if r.skip {
				dropped++
				continue
			}
			switch kind {
			case 0:
				out.ShapelessRecipes = append(out.ShapelessRecipes, x)
			case 5:
				out.UserDataShapelessRecipes = append(out.UserDataShapelessRecipes, protocol.UserDataShapelessRecipe{ShapelessRecipe: x})
			case 6:
				out.ShapelessChemistryRecipes = append(out.ShapelessChemistryRecipes, protocol.ShapelessChemistryRecipe{ShapelessRecipe: x})
			}
		case 1, 7:
			var x protocol.ShapedRecipe
			r.shaped(&x)
			if r.skip {
				dropped++
				continue
			}
			if kind == 1 {
				out.ShapedRecipes = append(out.ShapedRecipes, x)
			} else {
				out.ShapedChemistryRecipes = append(out.ShapedChemistryRecipes, protocol.ShapedChemistryRecipe{ShapedRecipe: x})
			}
		case 4:
			var x protocol.MultiRecipe
			x.Marshal(r)
			out.MultiRecipes = append(out.MultiRecipes, x)
		case 8:
			var x protocol.SmithingTransformRecipe
			r.String(&x.RecipeID)
			r.ItemDescriptorCount(&x.Template)
			r.ItemDescriptorCount(&x.Base)
			r.ItemDescriptorCount(&x.Addition)
			r.Item(&x.Result)
			r.String(&x.Block)
			r.Varuint32(&x.RecipeNetworkID)
			if r.skip {
				dropped++
				continue
			}
			out.SmithingTransformRecipes = append(out.SmithingTransformRecipes, x)
		case 9:
			var x protocol.SmithingTrimRecipe
			r.String(&x.RecipeID)
			r.ItemDescriptorCount(&x.Template)
			r.ItemDescriptorCount(&x.Base)
			r.ItemDescriptorCount(&x.Addition)
			r.String(&x.Block)
			r.Varuint32(&x.RecipeNetworkID)
			if r.skip {
				dropped++
				continue
			}
			out.SmithingTrimRecipes = append(out.SmithingTrimRecipes, x)
		case 2, 3:
			// Native CraftingData has no furnace recipe vector. Match the pinned
			// adapter's explicit behaviour; consume these legacy-only records.
			var id, metadata int32
			var output protocol.ItemStack
			var block string
			r.Varint32(&id)
			if kind == 3 {
				r.Varint32(&metadata)
			}
			r.Item(&output)
			r.String(&block)
			if r.skip {
				dropped++
			}
		default:
			return nil, fmt.Errorf("unknown 1001 recipe type %d", kind)
		}
	}
	protocol.Slice(r, &out.PotionRecipes)
	keptPotions := out.PotionRecipes[:0]
	for i := range out.PotionRecipes {
		r.skip = false
		x := out.PotionRecipes[i]
		x.InputPotionID = r.nativeItemID(x.InputPotionID)
		x.ReagentItemID = r.nativeItemID(x.ReagentItemID)
		x.OutputPotionID = r.nativeItemID(x.OutputPotionID)
		if r.skip {
			dropped++
			continue
		}
		keptPotions = append(keptPotions, x)
	}
	out.PotionRecipes = keptPotions
	protocol.Slice(r, &out.PotionContainerChangeRecipes)
	keptContainers := out.PotionContainerChangeRecipes[:0]
	for i := range out.PotionContainerChangeRecipes {
		r.skip = false
		x := out.PotionContainerChangeRecipes[i]
		x.InputItemID = r.nativeItemID(x.InputItemID)
		x.ReagentItemID = r.nativeItemID(x.ReagentItemID)
		x.OutputItemID = r.nativeItemID(x.OutputItemID)
		if r.skip {
			dropped++
			continue
		}
		keptContainers = append(keptContainers, x)
	}
	out.PotionContainerChangeRecipes = keptContainers
	r.reducerSkip = r.reducerSkip[:0]
	protocol.FuncSlice(r, &out.MaterialReducers, r.MaterialReducer)
	keptReducers := out.MaterialReducers[:0]
	for i := range out.MaterialReducers {
		if i < len(r.reducerSkip) && r.reducerSkip[i] {
			dropped++
			continue
		}
		keptReducers = append(keptReducers, out.MaterialReducers[i])
	}
	out.MaterialReducers = keptReducers
	r.Bool(&out.ClearRecipes)
	if dropped > 0 {
		slog.Warn("xvrelay: dropped recipes whose items are absent from the native registry (host addon)",
			"dropped", dropped, "unmappedRefs", r.unmapped,
			"shaped", len(out.ShapedRecipes), "shapeless", len(out.ShapelessRecipes),
			"smithing", len(out.SmithingTransformRecipes)+len(out.SmithingTrimRecipes),
			"potions", len(out.PotionRecipes), "reducers", len(out.MaterialReducers))
	}
	if buf.Len() != 0 {
		return nil, fmt.Errorf("1001 recipes have %d unread bytes", buf.Len())
	}
	return out, nil
}
