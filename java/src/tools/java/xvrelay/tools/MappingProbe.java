package xvrelay.tools;

import xvrelay.block.BlockMapper;
import xvrelay.block.Palette;

import java.nio.file.Path;

/**
 * Measures how much of each palette survives a round trip to another family.
 *
 * <p>The numbers this prints are the honest coverage figure for block
 * translation. A state that cannot be mapped becomes the visible
 * {@code minecraft:info_update} placeholder, so the fallback column is a real
 * user-visible cost, not a statistic.
 *
 * <pre>
 *   java -cp "lib/*:out" xvrelay.tools.MappingProbe [dataDir]
 * </pre>
 */
public final class MappingProbe {

    private static final int[][] PAIRS = {
            {1001, 2193}, {2168, 2193}, {2193, 1001}, {2193, 2168},
    };

    public static void main(String[] args) throws Exception {
        Path dataDir = args.length > 0
                ? Path.of(args[0])
                : Path.of("..", "internal", "blocktranslate", "data");

        Palette p1001 = Palette.load(1001, dataDir.resolve("1001.nbt"));
        Palette p2168 = Palette.load(2168, dataDir.resolve("2168.nbt"));
        Palette p2193 = Palette.load(2193, dataDir.resolve("2193.nbt"));
        // 2169 shares the 1.26.40-45 block schema, exactly as on the Go side.
        java.util.Map<Integer, Palette> palettes = java.util.Map.of(
                1001, p1001, 2168, p2168, 2169, p2168, 2193, p2193);

        int failures = 0;
        for (int[] pair : PAIRS) {
            Palette source = palettes.get(pair[0]);
            Palette target = palettes.get(pair[1]);
            BlockMapper mapper = BlockMapper.create(source, target, true);
            long fallbacks = mapper.fallbackCount();
            long total = source.size();
            double exactPct = 100.0 * (total - fallbacks) / total;
            System.out.printf("%d -> %-5d states=%-6d exact=%-6d reduced=%-5d fallback=%-5d (%.2f%% exact)%n",
                    pair[0], pair[1], total, total - fallbacks - mapper.reducedCount(),
                    mapper.reducedCount(), fallbacks, exactPct);
            if (fallbacks > total) {
                failures++;
            }
        }
        System.out.println(failures == 0 ? "mapping built for all pairs" : failures + " pair(s) failed");
        if (failures != 0) {
            System.exit(1);
        }
    }

    private MappingProbe() {
    }
}
