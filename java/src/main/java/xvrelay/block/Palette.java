package xvrelay.block;

import org.cloudburstmc.nbt.NBTInputStream;
import org.cloudburstmc.nbt.NbtMap;

import java.io.BufferedInputStream;
import java.io.DataInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.zip.GZIPInputStream;

/**
 * One pinned vanilla block palette, loaded from {@code data/<protocol>.nbt}.
 *
 * <p>The files come from CloudburstMC/Data at a pinned commit (see
 * {@code data/manifest.json}) and are gzipped big-endian NBT. Each entry pins
 * {@code network_id}, which lets {@link #load} re-derive every hash and fail
 * loudly if the table and the hashing code ever disagree. That check is the
 * whole point: a silently wrong hash produces a world made of placeholder
 * blocks, not a crash.
 *
 * <p>Runtime IDs are <em>positions in this list</em>, never the {@code block_id}
 * field — the two differ, and only the list position is what goes on the wire.
 */
public final class Palette {

    private final int protocolId;
    private final List<Entry> entries;
    private final Map<Integer, Integer> byHash;
    private final Map<String, Integer> byKey;
    private final int airRuntimeId;

    /** One block state at one runtime ID. */
    public record Entry(int runtimeId, String name, Map<String, Object> states, int hash) {
    }

    private Palette(int protocolId, List<Entry> entries, int airRuntimeId) {
        this.protocolId = protocolId;
        this.entries = List.copyOf(entries);
        this.airRuntimeId = airRuntimeId;
        Map<Integer, Integer> hashes = new HashMap<>(entries.size() * 2);
        Map<String, Integer> keys = new HashMap<>(entries.size() * 2);
        for (Entry entry : this.entries) {
            hashes.put(entry.hash(), entry.runtimeId());
            keys.put(key(entry.name(), entry.states()), entry.runtimeId());
        }
        this.byHash = hashes;
        this.byKey = keys;
    }

    /** Builds a stable string key for a state; only used for duplicate detection. */
    static String key(String name, Map<String, Object> states) {
        List<String> keys = new ArrayList<>(states.keySet());
        Collections.sort(keys);
        StringBuilder sb = new StringBuilder(name).append('[');
        for (String k : keys) {
            sb.append(k).append('=').append(states.get(k)).append(',');
        }
        return sb.append(']').toString();
    }

    public int protocolId() {
        return protocolId;
    }

    public int size() {
        return entries.size();
    }

    public Entry entry(int runtimeId) {
        return runtimeId >= 0 && runtimeId < entries.size() ? entries.get(runtimeId) : null;
    }

    public int airRuntimeId() {
        return airRuntimeId;
    }

    /** Runtime ID for a typed state hash, or -1 when the state is not in this palette. */
    public int runtimeIdByHash(int hash) {
        Integer id = byHash.get(hash);
        return id == null ? -1 : id;
    }

    /** Runtime ID for a name+states pair, or -1. */
    public int runtimeIdByName(String name, Map<String, Object> states) {
        Integer id = byKey.get(key(name, states));
        return id == null ? -1 : id;
    }

    public boolean hasHash(int hash) {
        return byHash.containsKey(hash);
    }

    public List<Entry> entries() {
        return entries;
    }

    /**
     * Loads and validates one palette.
     *
     * @throws IOException              when the file is missing or not valid NBT
     * @throws IllegalStateException    when a computed hash disagrees with the pinned network_id
     */
    public static Palette load(int protocolId, Path file) throws IOException {
        try (InputStream raw = Files.newInputStream(file);
             InputStream gz = new GZIPInputStream(new BufferedInputStream(raw));
             NBTInputStream nbt = new NBTInputStream(new DataInputStream(gz), false, false)) {
            Object root = nbt.readTag();
            if (!(root instanceof NbtMap map)) {
                throw new IOException("palette " + protocolId + ": root is not a compound");
            }
            Object blocks = map.get("blocks");
            if (!(blocks instanceof List<?> list) || list.isEmpty()) {
                throw new IOException("palette " + protocolId + ": no blocks");
            }

            List<Entry> entries = new ArrayList<>(list.size());
            int air = -1;
            for (int i = 0; i < list.size(); i++) {
                if (!(list.get(i) instanceof NbtMap block)) {
                    throw new IOException("palette " + protocolId + ": block " + i + " is not a compound");
                }
                String name = String.valueOf(block.get("name"));
                Map<String, Object> states = readStates(block);
                int pinned = ((Number) block.get("network_id")).intValue();
                int computed = StateHash.of(name, states);
                if (computed != pinned) {
                    throw new IllegalStateException(String.format(
                            "palette %d %s: computed hash %#x differs from pinned network_id %#x",
                            protocolId, name, computed, pinned));
                }
                entries.add(new Entry(i, name, states, computed));
                if ("minecraft:air".equals(name)) {
                    air = i;
                }
            }
            if (air < 0) {
                throw new IOException("palette " + protocolId + ": no minecraft:air");
            }
            return new Palette(protocolId, entries, air);
        }
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> readStates(NbtMap block) {
        Object raw = block.get("states");
        if (!(raw instanceof Map<?, ?> map) || map.isEmpty()) {
            return Map.of();
        }
        Map<String, Object> out = new LinkedHashMap<>(map.size() * 2);
        for (Map.Entry<?, ?> e : ((Map<Object, Object>) map).entrySet()) {
            out.put(String.valueOf(e.getKey()), e.getValue());
        }
        return out;
    }

    @Override
    public String toString() {
        return "Palette[" + protocolId + ", " + entries.size() + " states]";
    }
}
