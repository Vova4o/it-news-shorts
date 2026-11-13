#!/usr/bin/env bash
# Builds the reproducible 1080x1920 test clip used for the YouTube integration
# check: a plain background with the label "IT NEWS API TEST".
#
# No music and no third-party material are used. The label is rendered by the
# first available tool, because some FFmpeg builds ship without libfreetype and
# therefore without the drawtext filter:
#   1. ffmpeg drawtext (single pass)
#   2. a transparent PNG label from Python PIL, composited with ffmpeg overlay
#   3. a transparent PNG label from ImageMagick, composited the same way
#   4. fail with an explicit error if no text renderer is available
set -euo pipefail

OUTPUT="${1:-testdata/test-short.mp4}"
DURATION="${DURATION:-7}"
WIDTH=1080
HEIGHT=1920
LABEL="IT NEWS API TEST"

if ! command -v ffmpeg >/dev/null 2>&1; then
	echo "ffmpeg is required to build the test video" >&2
	exit 3
fi

mkdir -p "$(dirname "$OUTPUT")"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

has_filter() {
	ffmpeg -hide_banner -filters 2>/dev/null | awk -v name="$1" '$2 == name { found = 1 } END { exit !found }'
}

# Renders the label into a transparent PNG. Returns non-zero when no tool works.
render_label_png() {
	local target="$1"

	if command -v python3 >/dev/null 2>&1; then
		if python3 - "$target" "$WIDTH" "$HEIGHT" "$LABEL" <<'PY'
import sys
from PIL import Image, ImageDraw, ImageFont

target, width, height, text = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
candidates = [
    "/System/Library/Fonts/Supplemental/Arial.ttf",
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
    "/System/Library/Fonts/Supplemental/Verdana.ttf",
    "/System/Library/Fonts/Helvetica.ttc",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
]
font = None
for path in candidates:
    try:
        font = ImageFont.truetype(path, 64)
        break
    except Exception:
        continue
if font is None:
    raise SystemExit(1)

image = Image.new("RGBA", (width, height), (0, 0, 0, 0))
draw = ImageDraw.Draw(image)
box = draw.textbbox((0, 0), text, font=font)
text_width, text_height = box[2] - box[0], box[3] - box[1]
draw.text(
    ((width - text_width) / 2 - box[0], (height - text_height) / 2 - box[1]),
    text,
    font=font,
    fill=(255, 255, 255, 255),
)
image.save(target)
PY
		then
			return 0
		fi
	fi

	if command -v magick >/dev/null 2>&1 || command -v convert >/dev/null 2>&1; then
		local tool="magick"
		command -v magick >/dev/null 2>&1 || tool="convert"
		if "$tool" -size "${WIDTH}x${HEIGHT}" xc:none -gravity center \
			-pointsize 64 -fill white -annotate 0 "$LABEL" "$target" 2>/dev/null; then
			return 0
		fi
	fi

	return 1
}

if has_filter drawtext; then
	FONT="${FONT:-}"
	if [ -z "$FONT" ]; then
		for candidate in \
			/System/Library/Fonts/Supplemental/Arial.ttf \
			/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf; do
			if [ -f "$candidate" ]; then
				FONT="$candidate"
				break
			fi
		done
	fi
	DRAWTEXT="drawtext=text='${LABEL}':fontcolor=white:fontsize=64:x=(w-text_w)/2:y=(h-text_h)/2"
	if [ -n "$FONT" ]; then
		DRAWTEXT="drawtext=fontfile='${FONT}':text='${LABEL}':fontcolor=white:fontsize=64:x=(w-text_w)/2:y=(h-text_h)/2"
	fi
	ffmpeg -hide_banner -loglevel error -y \
		-f lavfi -i "color=c=0x101820:s=${WIDTH}x${HEIGHT}:d=${DURATION}:r=30" \
		-f lavfi -i "anullsrc=channel_layout=stereo:sample_rate=48000" \
		-t "$DURATION" \
		-vf "$DRAWTEXT" \
		-c:v libx264 -profile:v high -level 4.1 -pix_fmt yuv420p -preset veryfast -crf 23 \
		-c:a aac -b:a 128k -shortest \
		-movflags +faststart \
		"$OUTPUT"
	echo "wrote $OUTPUT (label via ffmpeg drawtext)"
	exit 0
fi

LABEL_PNG="$WORKDIR/label.png"
if render_label_png "$LABEL_PNG" && has_filter overlay; then
	ffmpeg -hide_banner -loglevel error -y \
		-f lavfi -i "color=c=0x101820:s=${WIDTH}x${HEIGHT}:d=${DURATION}:r=30" \
		-loop 1 -i "$LABEL_PNG" \
		-f lavfi -i "anullsrc=channel_layout=stereo:sample_rate=48000" \
		-filter_complex "[0:v][1:v]overlay=0:0[out]" \
		-map "[out]" -map 2:a \
		-t "$DURATION" \
		-c:v libx264 -profile:v high -level 4.1 -pix_fmt yuv420p -preset veryfast -crf 23 \
		-c:a aac -b:a 128k -shortest \
		-movflags +faststart \
		"$OUTPUT"
	echo "wrote $OUTPUT (label composited from PNG)"
	exit 0
fi

echo "error: cannot render required label '$LABEL': neither ffmpeg drawtext filter nor Python PIL / ImageMagick with ffmpeg overlay is available" >&2
exit 1
