package xvrelay.block;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Maps block runtime IDs from one protocol family to another.
 *
 * <h2>Why the state, not the ordinal</h2>
 * Runtime IDs are assigned by the server when it sends {@code StartGame}. They
 * are dense and version specific: the same number means a different block in
 * 1.26.30 and 1.26.51. Mapping by ordinal would silently turn stone into wool.
 * So every source ID is first resolved to its block state, and only then looked
 * up in the target palette.
 *
 * <h2>Two passes</h2>
 * <ol>
 *   <li><b>Exact</b> — identical name and identical property set. Always
 *       correct when it hits.</li>
 *   <li><b>Reduced</b> — same name, properties that do not exist in the target
 *       family dropped (for example a new "pillar_axis" state added in a later
 *       version). Only accepted when exactly one target candidate remains;
 *       ambiguity is treated as a miss rather than guessed.</li>
 * </ol>
 *
 * <h2>Never air</h2>
 * A miss becomes {@code minecraft:info_update}, a visible "missing block"
 * texture, and is counted in {@link #fallbackCount()}. Sending air would erase
 * terrain without telling anyone; sending the host's raw ID would make the
 * client read an unrelated block or crash on an out-of-range ID.
 */
public final class BlockMapper {

    private static final String PLACEHOLDER = "minecraft:info_update";

    private final Palette source;
    private final Palette target;
    private final boolean hashed;
    private final Map<Integer, Integer> bySourceId = new HashMap<>();
    private final Map<String, List<Palette.Entry>> targetByName = new HashMap<>();
    private final int fallbackId;
    private long fallbacks;
    private long reduced;

    private BlockMapper(Palette source, Palette target, boolean hashed) {
        this.source = source;
        this.target = target;
        this.hashed = hashed;

        for (Palette.Entry entry : target.entries()) {
            targetByName.computeIfAbsent(entry.name(), k -> new ArrayList<>(2)).add(entry);
        }
        int placeholder = target.runtimeIdByName(PLACEHOLDER, Map.of());
        if (placeholder < 0) {
            throw new IllegalStateException("target palette has no " + PLACEHOLDER + " placeholder");
        }
        this.fallbackId = placeholder;
        build();
    }

    /**
     * Builds a mapper.
     *
     * @param sourceProtocol protocol id of the sending side
     * @param targetProtocol protocol id of the receiving side
     * @param hashed         whether the wire carries typed state hashes instead of runtime IDs
     */
    public static BlockMapper create(Palette source, Palette target, boolean hashed) {
        return new BlockMapper(source, target, hashed);
    }

    private void build() {
        for (Palette.Entry entry : source.entries()) {
            int mapped = resolve(entry);
            if (mapped < 0) {
                mapped = fallbackId;
                fallbacks++;
            }
            bySourceId.put(hashed ? entry.hash() : entry.runtimeId(),
                    hashed ? target.entry(mapped).hash() : mapped);
        }
    }

    /** Returns the target runtime ID for a source entry, or -1 when nothing fits. */
    private int resolve(Palette.Entry entry) {
        int exact = target.runtimeIdByName(entry.name(), entry.states());
        if (exact >= 0) {
            return exact;
        }
        List<Palette.Entry> candidates = targetByName.get(entry.name());
        if (candidates == null) {
            return -1;
        }
        // Reduced pass: keep candidates whose property values agree on every
        // property the two families share.
        List<Palette.Entry> compatible = new ArrayList<>(2);
        for (Palette.Entry candidate : candidates) {
            if (compatible(entry.states(), candidate.states())) {
                compatible.add(candidate);
            }
        }
        if (compatible.size() == 1) {
            reduced++;
            return compatible.get(0).runtimeId();
        }
        if (compatible.size() > 1) {
            // Still ambiguous. This happens when the target family introduced
            // properties the source family does not have: 1.26.50 added
            // minecraft:connection_{north,east,south,west} to panes and stairs,
            // so one 1.26.30 pane state corresponds to 56 candidates. Measured:
            // 584 of 16913 states are in this situation for 1001 -> 2193.
            //
            // The neutral variant — every extra property at its zero value —
            // is the one the old state actually means: a pane with no
            // neighbours, a stair with no connected blocks. Anything else
            // would render a wall of joined panes that the host never had.
            Palette.Entry neutral = null;
            for (Palette.Entry candidate : compatible) {
                if (!allExtrasNeutral(entry.states(), candidate.states())) {
                    continue;
                }
                if (neutral != null) {
                    neutral = null; // two neutral candidates: still ambiguous
                    break;
                }
                neutral = candidate;
            }
            if (neutral != null) {
                reduced++;
                return neutral.runtimeId();
            }
        }
        return -1; // no candidate, or ambiguous: never guess
    }

    /** True when every property only the target has carries its zero value. */
    private static boolean allExtrasNeutral(Map<String, Object> source, Map<String, Object> target) {
        for (Map.Entry<String, Object> e : target.entrySet()) {
            if (source.containsKey(e.getKey())) {
                continue;
            }
            if (!isZero(e.getValue())) {
                return false;
            }
        }
        return true;
    }

    private static boolean isZero(Object value) {
        return switch (value) {
            case Byte b -> b == 0;
            case Short s -> s == 0;
            case Integer i -> i == 0;
            case String s -> s.isEmpty();
            case Boolean b -> !b;
            case null, default -> false;
        };
    }

    private static boolean compatible(Map<String, Object> source, Map<String, Object> target) {
        for (Map.Entry<String, Object> e : target.entrySet()) {
            Object other = source.get(e.getKey());
            if (other != null && !other.equals(e.getValue())) {
                return false;
            }
        }
        return true;
    }

    /**
     * Translates one wire ID.
     *
     * <p>An ID that is not in the source palette is a resource-pack custom block
     * the host declared at runtime. Those cannot be resolved from the pinned
     * tables, so they become the placeholder — a wrong texture is recoverable, a
     * dropped session is not.
     */
    public int map(int wireId) {
        Integer mapped = bySourceId.get(wireId);
        if (mapped != null) {
            return mapped;
        }
        fallbacks++;
        return hashed ? target.entry(fallbackId).hash() : fallbackId;
    }

    public boolean knows(int wireId) {
        return bySourceId.containsKey(wireId);
    }

    public long fallbackCount() {
        return fallbacks;
    }

    public long reducedCount() {
        return reduced;
    }

    public Palette source() {
        return source;
    }

    public Palette target() {
        return target;
    }

    /** The wire ID used in place of an unmappable block. */
    public int placeholderId() {
        return hashed ? target.entry(fallbackId).hash() : fallbackId;
    }
}
