package xvrelay.tools;

import org.cloudburstmc.protocol.bedrock.codec.BedrockCodec;

/**
 * Prints the protocol versions and packet counts of the codecs the relay uses.
 *
 * This is the first thing to run when the dependency set is refreshed: if a
 * codec no longer loads, every cross-version claim in the README is void.
 *
 * <pre>
 *   java -cp "lib/*:out" xvrelay.tools.CodecProbe
 * </pre>
 */
public final class CodecProbe {

    private record Entry(int id, String className) {
    }

    private static final Entry[] CODECS = {
            new Entry(1001, "org.cloudburstmc.protocol.bedrock.codec.v1001.Bedrock_v1001"),
            new Entry(2168, "org.cloudburstmc.protocol.bedrock.codec.v2168.Bedrock_v2168"),
            new Entry(2169, "org.cloudburstmc.protocol.bedrock.codec.v2169.Bedrock_v2169"),
            new Entry(2193, "org.cloudburstmc.protocol.bedrock.codec.v2193.Bedrock_v2193"),
    };

    public static void main(String[] args) throws Exception {
        int failures = 0;
        for (Entry entry : CODECS) {
            try {
                Class<?> type = Class.forName(entry.className());
                Object codec = type.getField("CODEC").get(null);
                if (!(codec instanceof BedrockCodec bedrock)) {
                    System.out.printf("proto %-5d FAIL  CODEC is not a BedrockCodec%n", entry.id());
                    failures++;
                    continue;
                }
                // BedrockCodec has no public "packet count" accessor, so probe
                // the id space. Holes are fine — Bedrock's id map is sparse.
                int packets = 0;
                for (int id = 0; id <= 0xFF; id++) {
                    if (bedrock.getPacketDefinition(id) != null) {
                        packets++;
                    }
                }
                System.out.printf(
                        "proto %-5d ok    protocol=%d  minecraftVersion=%s  raknet=%d  packets=%d%n",
                        entry.id(),
                        bedrock.getProtocolVersion(),
                        bedrock.getMinecraftVersion(),
                        bedrock.getRaknetProtocolVersion(),
                        packets);
            } catch (Throwable t) {
                System.out.printf("proto %-5d FAIL  %s%n", entry.id(), t);
                failures++;
            }
        }
        System.out.println(failures == 0 ? "all codecs loaded" : failures + " codec(s) failed");
        if (failures != 0) {
            System.exit(1);
        }
    }

    private CodecProbe() {
    }
}
