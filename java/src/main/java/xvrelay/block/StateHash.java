package xvrelay.block;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;

/**
 * FNV-1a over the sorted, typed little-endian NBT encoding of a block state.
 *
 * <p>This reproduces the "hashed network palette" that Bedrock uses when a
 * client and server negotiate block states by hash instead of by ordinal. It
 * must match byte for byte — a single byte of difference turns every lookup
 * into a miss, and a miss means the client renders the placeholder instead of
 * the real block.
 *
 * <p>Encoding (little-endian throughout, which is NOT how NBT is normally
 * written):
 *
 * <pre>
 *   0A 00 00                 root compound, unnamed
 *   08 00 04 "name"  &lt;str&gt;   string tag
 *   0A 00 06 "states"        compound tag
 *     &lt;one typed tag per property, keys sorted&gt;
 *   00                       end of states
 *   00                       end of root
 * </pre>
 *
 * <p>Property values keep their NBT type: a string stays TAG_String, a boolean
 * is TAG_Byte, and so on. Two states that differ only by the Java type of a
 * value therefore hash differently — which is correct, because the game does
 * the same.
 */
public final class StateHash {

    /** Hash the game assigns to {@code minecraft:unknown}; never computed. */
    public static final int UNKNOWN_HASH = 0xFFFFFFFE;

    private static final int FNV_OFFSET_BASIS = 0x811C9DC5;
    private static final int FNV_PRIME = 0x01000193;

    /** Computes the typed state hash for one block state. */
    public static int of(String name, Map<String, Object> properties) {
        if ("minecraft:unknown".equals(name)) {
            return UNKNOWN_HASH;
        }
        List<String> keys = new ArrayList<>(properties.keySet());
        Collections.sort(keys);

        Writer out = new Writer(64);
        out.raw(0x0A).raw(0x00).raw(0x00); // unnamed root compound
        out.tag(Tag.STRING).name("name").string(name);
        out.tag(Tag.COMPOUND).name("states");

        for (String key : keys) {
            Object value = properties.get(key);
            switch (value) {
                case String s -> out.tag(Tag.STRING).name(key).string(s);
                case Boolean b -> out.tag(Tag.BYTE).name(key).raw(b ? 1 : 0);
                case Byte b -> out.tag(Tag.BYTE).name(key).raw(b);
                case Short s -> out.tag(Tag.SHORT).name(key).le16(s);
                case Integer i -> out.tag(Tag.INT).name(key).le32(i);
                default -> throw new IllegalArgumentException(
                        "unsupported state value for " + key + ": " + value.getClass().getName());
            }
        }
        out.raw(0x00); // end states
        out.raw(0x00); // end root
        return out.fnv1a();
    }

    private enum Tag {
        BYTE(1), SHORT(2), INT(3), STRING(8), COMPOUND(10);

        final int id;

        Tag(int id) {
            this.id = id;
        }
    }

    /** Minimal byte sink — avoids the boxing of ByteArrayOutputStream. */
    private static final class Writer {
        private byte[] buf;
        private int len;

        Writer(int capacity) {
            buf = new byte[capacity];
        }

        private void ensure(int extra) {
            if (len + extra <= buf.length) {
                return;
            }
            int next = Math.max(buf.length * 2, len + extra);
            byte[] grown = new byte[next];
            System.arraycopy(buf, 0, grown, 0, len);
            buf = grown;
        }

        Writer raw(int b) {
            ensure(1);
            buf[len++] = (byte) b;
            return this;
        }

        Writer tag(Tag tag) {
            return raw(tag.id);
        }

        Writer name(String value) {
            return string(value);
        }

        Writer string(String value) {
            byte[] bytes = value.getBytes(StandardCharsets.UTF_8);
            if (bytes.length > 0xFFFF) {
                throw new IllegalArgumentException("NBT string too long: " + bytes.length);
            }
            ensure(2 + bytes.length);
            buf[len++] = (byte) (bytes.length & 0xFF);
            buf[len++] = (byte) ((bytes.length >>> 8) & 0xFF);
            System.arraycopy(bytes, 0, buf, len, bytes.length);
            len += bytes.length;
            return this;
        }

        Writer le16(short value) {
            ensure(2);
            buf[len++] = (byte) (value & 0xFF);
            buf[len++] = (byte) ((value >>> 8) & 0xFF);
            return this;
        }

        Writer le32(int value) {
            ensure(4);
            buf[len++] = (byte) (value & 0xFF);
            buf[len++] = (byte) ((value >>> 8) & 0xFF);
            buf[len++] = (byte) ((value >>> 16) & 0xFF);
            buf[len++] = (byte) ((value >>> 24) & 0xFF);
            return this;
        }

        int fnv1a() {
            int hash = FNV_OFFSET_BASIS;
            for (int i = 0; i < len; i++) {
                hash ^= buf[i] & 0xFF;
                hash *= FNV_PRIME;
            }
            return hash;
        }
    }

    private StateHash() {
    }
}
