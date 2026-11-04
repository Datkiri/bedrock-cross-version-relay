package xvrelay.tools;

import xvrelay.block.Palette;

import java.nio.file.Files;
import java.nio.file.Path;

/**
 * Loads every pinned palette and reports its size.
 *
 * <p>This is the fastest way to tell whether {@link xvrelay.block.StateHash}
 * still matches the game: {@link Palette#load} re-derives each hash and refuses
 * to load when one disagrees with the pinned {@code network_id}. If this prints
 * three sizes, the hashing code is byte-exact.
 *
 * <pre>
 *   java -cp "lib/*:out" xvrelay.tools.PaletteProbe [dataDir]
 * </pre>
 */
public final class PaletteProbe {

    private static final int[] PROTOCOLS = {1001, 2168, 2193};

    public static void main(String[] args) throws Exception {
        Path dataDir = args.length > 0
                ? Path.of(args[0])
                : Path.of("..", "internal", "blocktranslate", "data");
        if (!Files.isDirectory(dataDir)) {
            System.err.println("data directory not found: " + dataDir.toAbsolutePath());
            System.exit(2);
        }
        int failures = 0;
        for (int protocol : PROTOCOLS) {
            Path file = dataDir.resolve(protocol + ".nbt");
            try {
                Palette palette = Palette.load(protocol, file);
                System.out.printf("proto %-5d ok    states=%-6d air=%d  %s%n",
                        protocol, palette.size(), palette.airRuntimeId(), file.getFileName());
            } catch (Throwable t) {
                System.out.printf("proto %-5d FAIL  %s%n", protocol, t);
                failures++;
            }
        }
        System.out.println(failures == 0 ? "all palettes verified" : failures + " palette(s) failed");
        if (failures != 0) {
            System.exit(1);
        }
    }

    private PaletteProbe() {
    }
}
