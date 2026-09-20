"""Monkey-patch Anemll DeepseekV4ForCausalLM for Vision-Exp images.

Called at the end of ``vllm/models/deepseek_v4/nvidia/model.py`` after the
stock classes are defined. Video is not registered: the checkpoint has no
video tower.
"""
from __future__ import annotations

from typing import Any

import torch
from torch import nn

from .image_processor import (
    IMAGE,
    is_unregistered_router_bias,
    is_vision_exp_weight_name,
    vision_args_from_config,
)
from .processor import IMAGE_PLACEHOLDER, register_vision_exp_processor
from .vision import Aligner, ViT

VISION_MAPPER_PREFIXES = {
    "vision.": "model.vision.",
    "aligner.": "model.aligner.",
    "image_start": "model.image_start",
    "image_end": "model.image_end",
    "image_newline": "model.image_newline",
    "image_pad": "model.image_pad",
}
VISION_MAPPER_SUFFIXES = {
    ".ffn.gate.bias_vl": ".ffn.gate.e_score_correction_bias_vl",
}


def _as_rows(value: Any) -> list[Any]:
    if value is None:
        return []
    if isinstance(value, (list, tuple)):
        return list(value)
    if isinstance(value, torch.Tensor):
        if value.ndim == 0:
            return [value]
        return [value[i] for i in range(value.shape[0])]
    return [value]


def _scalar(value: Any) -> int:
    if hasattr(value, "reshape"):
        return int(value.reshape(-1)[0].item())
    return int(value)


def _extend_weights_mapper(mapper):
    from vllm.model_executor.models.utils import WeightsMapper

    extra = WeightsMapper(
        orig_to_new_prefix=dict(VISION_MAPPER_PREFIXES),
        orig_to_new_suffix=dict(VISION_MAPPER_SUFFIXES),
    )
    return mapper | extra


def _install_vision_tower(model: nn.Module, config) -> None:
    if getattr(model, "vision", None) is not None:
        return
    args = vision_args_from_config(config)
    if args.vision_n_layers <= 0:
        model.vision = None
        model.aligner = None
        return
    model.vision = ViT(args)
    model.aligner = Aligner(args)
    hidden = args.dim
    model.image_start = nn.Parameter(torch.empty(hidden))
    model.image_end = nn.Parameter(torch.empty(hidden))
    model.image_newline = nn.Parameter(torch.empty(hidden))
    model.image_pad = nn.Parameter(torch.empty(hidden))


@torch.inference_mode()
def encode_image(model: nn.Module, patches: torch.Tensor, n_vit_h: int, n_vit_w: int):
    return model.aligner(model.vision(patches, n_vit_h, n_vit_w), n_vit_h, n_vit_w)


def merge_one_image(
    model: nn.Module,
    patches: torch.Tensor,
    n_vit_h: int,
    n_vit_w: int,
    types: torch.Tensor,
    perm: torch.Tensor,
    num_tokens: int,
) -> torch.Tensor:
    types = types[:num_tokens].to(dtype=torch.long)
    perm = perm.to(dtype=torch.long)
    perm = perm[perm >= 0]
    n_patches = int(n_vit_h) * int(n_vit_w)
    patches = patches[:n_patches]
    device = next(model.vision.parameters()).device
    dtype = next(model.aligner.parameters()).dtype
    patches = patches.to(device=device, dtype=dtype)
    embeds = encode_image(model, patches, int(n_vit_h), int(n_vit_w))[perm.to(device)]
    params = torch.stack(
        [
            model.image_start,
            model.image_pad,
            model.image_pad,
            model.image_newline,
            model.image_end,
        ]
    ).to(device=device, dtype=embeds.dtype)
    block = params[types.to(device)]
    image_mask = types.to(device) == IMAGE
    if int(image_mask.sum()) != int(embeds.shape[0]):
        raise RuntimeError(
            f"Vision-Exp layout mismatch: {int(image_mask.sum())} IMAGE slots, "
            f"{int(embeds.shape[0])} aligner tokens"
        )
    block = block.clone()
    block[image_mask] = embeds.to(block.dtype)
    return block


def embed_multimodal(self, **kwargs: object):
    pixel_values = kwargs.get("pixel_values")
    if pixel_values is None:
        return []
    pixels = _as_rows(pixel_values)
    n_vit_h = _as_rows(kwargs.get("n_vit_h"))
    n_vit_w = _as_rows(kwargs.get("n_vit_w"))
    types = _as_rows(kwargs.get("types"))
    perm = _as_rows(kwargs.get("perm"))
    num_tokens = _as_rows(kwargs.get("num_tokens"))
    inner = self.model
    if getattr(inner, "vision", None) is None:
        raise RuntimeError("Vision-Exp tower was not constructed on DeepseekV4Model")
    out = []
    for i, patches in enumerate(pixels):
        out.append(
            merge_one_image(
                inner,
                patches,
                _scalar(n_vit_h[i]),
                _scalar(n_vit_w[i]),
                types[i],
                perm[i],
                _scalar(num_tokens[i]),
            )
        )
    return out


@classmethod
def get_placeholder_str(cls, modality: str, i: int) -> str | None:
    if modality.startswith("image"):
        return IMAGE_PLACEHOLDER
    raise ValueError(
        f"DeepSeek-V4-Flash-Vision-Exp supports images only, got {modality!r}"
    )


def apply_vision_exp(
    *,
    DeepseekV4Model,
    DeepseekV4ForCausalLM,
    DeepseekV4MoE,
) -> None:
    from vllm.model_executor.models.interfaces import SupportsMultiModal

    orig_model_init = DeepseekV4Model.__init__

    def model_init(self, *, vllm_config, prefix: str = ""):
        orig_model_init(self, vllm_config=vllm_config, prefix=prefix)
        _install_vision_tower(self, vllm_config.model_config.hf_config)

    DeepseekV4Model.__init__ = model_init
    DeepseekV4Model.encode_image = lambda self, patches, n_h, n_w: encode_image(
        self, patches, n_h, n_w
    )

    orig_model_load = DeepseekV4Model.load_weights

    def model_load_weights(self, weights):
        try:
            from vllm.model_executor.model_loader.weight_utils import (
                default_weight_loader,
            )
        except ImportError:
            default_weight_loader = None
        params_dict = dict(self.named_parameters())
        loaded: set[str] = set()

        def _filtered():
            for name, weight in weights:
                if is_unregistered_router_bias(name, params_dict):
                    continue
                if not is_vision_exp_weight_name(name):
                    yield name, weight
                    continue
                if name not in params_dict:
                    raise KeyError(
                        f"Vision-Exp weight {name!r} has no module parameter; "
                        "the ViT/Aligner tower was not constructed"
                    )
                param = params_dict[name]
                loader = getattr(param, "weight_loader", None) or default_weight_loader
                if loader is not None:
                    loader(param, weight)
                else:
                    param.data.copy_(weight)
                loaded.add(name)

        loaded |= orig_model_load(self, _filtered())
        return loaded

    DeepseekV4Model.load_weights = model_load_weights

    orig_moe_init = DeepseekV4MoE.__init__

    def moe_init(self, vllm_config, prefix: str = ""):
        orig_moe_init(self, vllm_config, prefix)
        config = vllm_config.model_config.hf_config
        if getattr(config, "vision_n_layers", 0) > 0:
            self.gate.e_score_correction_bias_vl = nn.Parameter(
                torch.empty(config.n_routed_experts, dtype=torch.float32),
                requires_grad=False,
            )

    DeepseekV4MoE.__init__ = moe_init

    orig_lm_init = DeepseekV4ForCausalLM.__init__

    def lm_init(self, *, vllm_config, prefix: str = ""):
        orig_lm_init(self, vllm_config=vllm_config, prefix=prefix)
        self.hf_to_vllm_mapper = _extend_weights_mapper(self.hf_to_vllm_mapper)

    DeepseekV4ForCausalLM.__init__ = lm_init
    DeepseekV4ForCausalLM.hf_to_vllm_mapper = _extend_weights_mapper(
        DeepseekV4ForCausalLM.hf_to_vllm_mapper
    )

    orig_lm_embed = DeepseekV4ForCausalLM.embed_input_ids

    def embed_input_ids(
        self,
        input_ids: torch.Tensor,
        multimodal_embeddings: Any = None,
        *,
        is_multimodal: Any = None,
    ):
        text_embeds = orig_lm_embed(self, input_ids)
        if multimodal_embeddings is None:
            return text_embeds
        try:
            empty = len(multimodal_embeddings) == 0
        except TypeError:
            empty = False
        if empty:
            return text_embeds
        from vllm.model_executor.models.interfaces import _require_is_multimodal
        from vllm.model_executor.models.utils import _merge_multimodal_embeddings

        return _merge_multimodal_embeddings(
            inputs_embeds=text_embeds,
            multimodal_embeddings=multimodal_embeddings,
            is_multimodal=_require_is_multimodal(is_multimodal),
        )

    DeepseekV4ForCausalLM.embed_input_ids = embed_input_ids

    if SupportsMultiModal not in DeepseekV4ForCausalLM.__mro__:
        DeepseekV4ForCausalLM.__bases__ = (
            DeepseekV4ForCausalLM.__bases__[0],
            SupportsMultiModal,
            *DeepseekV4ForCausalLM.__bases__[1:],
        )
    DeepseekV4ForCausalLM.supports_multimodal = True
    DeepseekV4ForCausalLM.supports_multimodal_raw_input_only = False
    # Hash MoE (layers 0–2) looks up tid2eid with input_ids. The MM runner
    # otherwise sets input_ids=None whenever inputs_embeds is present.
    DeepseekV4ForCausalLM.requires_raw_input_tokens = True
    DeepseekV4ForCausalLM.get_placeholder_str = get_placeholder_str
    DeepseekV4ForCausalLM.embed_multimodal = embed_multimodal

    # SupportsMultiModal.get_language_model() returns the first child with
    # embed_input_ids (DeepseekV4Model). DSpark/Eagle3 then read `.model` on
    # that child. This class *is* the LM wrapper, same as 0731.
    def get_language_model(self):
        return self

    def set_aux_hidden_state_layers(self, layers):
        self.model._set_aux_hidden_state_layers(layers)

    def get_eagle3_default_aux_hidden_state_layers(self):
        num_layers = len(self.model.layers)
        return (2, num_layers // 2, num_layers - 3)

    DeepseekV4ForCausalLM.get_language_model = get_language_model
    DeepseekV4ForCausalLM.set_aux_hidden_state_layers = set_aux_hidden_state_layers
    DeepseekV4ForCausalLM.get_eagle3_default_aux_hidden_state_layers = (
        get_eagle3_default_aux_hidden_state_layers
    )
    register_vision_exp_processor(DeepseekV4ForCausalLM)
